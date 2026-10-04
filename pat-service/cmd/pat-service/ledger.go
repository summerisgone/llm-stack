package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// Request ledger (docs/adr/0022 section 6): one qos_events row per chat or
// embeddings request that pat-service forwarded, with server timestamps,
// outcome and usage quality. No prompt or answer content is kept.

type tokenUsage struct {
	model      string
	prompt     int64
	cached     int64
	completion int64
}

// requestRecord collects one request's ledger row while it is proxied. It
// is only touched by the request's own goroutine.
type requestRecord struct {
	requestID string
	source    string // "pat" or "agent"
	owner     string
	ownerName string
	tokenID   string
	tokenName string
	sessionID string
	model     string
	band      string
	kind      string // "chat", "embeddings", or "" when not recorded

	receivedAt    time.Time
	dispatchedAt  time.Time
	firstOutputAt time.Time
	lastOutputAt  time.Time
	status        int
	streaming     bool
	usage         *tokenUsage
}

func ledgerKind(path string) string {
	switch {
	case strings.Contains(path, "/chat/completions"):
		return "chat"
	case strings.Contains(path, "/embeddings"):
		return "embeddings"
	}
	return ""
}

// requestOutcome classifies a finished request. A failure to deliver the
// body counts as cancelled when the client went away, otherwise as error.
func requestOutcome(r *http.Request, rec *requestRecord, copyErr error) string {
	switch {
	case rec.status == 0 || rec.status >= 500:
		return "error"
	case rec.status == http.StatusTooManyRequests:
		return "rejected"
	case rec.status >= 400:
		return "client_error"
	case r.Context().Err() != nil:
		return "cancelled"
	case copyErr != nil:
		return "error"
	}
	return "ok"
}

// finishRequest runs after the response was delivered (or failed): it feeds
// usage into the QoS spend tracker and writes the ledger row.
func (a *app) finishRequest(r *http.Request, rec *requestRecord, copyErr error) {
	if rec.kind == "" {
		return
	}
	finishedAt := time.Now()
	// Both writes outlive a cancelled client. Each has its own bound, so a
	// slow Valkey cannot cost the ledger row.
	detached := context.WithoutCancel(r.Context())
	if u := rec.usage; u != nil {
		if u.model == "" {
			u.model = rec.model
		}
		switch {
		case rec.kind == "embeddings" && u.prompt > 0:
			a.qos.Metrics().EmbeddingTokensTotal.WithLabelValues(rec.owner, u.model).Add(float64(u.prompt))
		case rec.kind == "chat" && (u.prompt > 0 || u.completion > 0):
			ctx, cancel := context.WithTimeout(detached, a.cfg.qosFingerprintTimeout)
			a.qos.RecordCost(ctx, rec.owner, u.model, u.prompt, u.cached, u.completion)
			cancel()
		}
	}
	ctx, cancel := context.WithTimeout(detached, a.cfg.qosEventTimeout)
	defer cancel()
	a.insertLedger(ctx, rec, requestOutcome(r, rec, copyErr), finishedAt)
}

func nullTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func (a *app) insertLedger(ctx context.Context, rec *requestRecord, outcome string, finishedAt time.Time) {
	if a.db == nil { // unit tests construct an app with no live pat-db
		return
	}
	model, quality := rec.model, "unknown"
	var prompt, cached, completion int64
	if u := rec.usage; u != nil {
		model, quality = u.model, "actual"
		prompt, cached, completion = u.prompt, u.cached, u.completion
	}
	sessionID := rec.sessionID
	if rec.kind == "embeddings" {
		sessionID = ""
	}
	band := rec.band
	if rec.kind == "embeddings" {
		band = "embeddings"
	}
	var status *int
	if rec.status != 0 {
		status = &rec.status
	}
	_, err := a.db.Exec(ctx, `INSERT INTO qos_events
		(owner_issuer, owner_subject, owner_name, token_id, token_name, session_id, model, band,
		 prompt_tokens, cached_tokens, completion_tokens, cost_amount,
		 request_id, source, outcome, http_status, streaming, usage_quality,
		 received_at, dispatched_at, first_output_at, last_output_at, finished_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)`,
		a.cfg.issuer, rec.owner, rec.ownerName, rec.tokenID, rec.tokenName, sessionID, model, band,
		prompt, cached, completion, a.pricing.cost(model, prompt, cached, completion),
		rec.requestID, rec.source, outcome, status, rec.streaming, quality,
		nullTime(rec.receivedAt), nullTime(rec.dispatchedAt), nullTime(rec.firstOutputAt), nullTime(rec.lastOutputAt), finishedAt)
	if err != nil {
		log.Printf("ledger insert: %v", err)
	}
}

// sseMaxLine bounds one buffered SSE line. A longer line (a huge tool-call
// chunk) is passed through untouched but not inspected.
const sseMaxLine = 1 << 20

// sseLedgerReader passes a streamed chat completion through byte for byte
// while it inspects each complete `data:` event: the first and last event
// carrying output (text, reasoning or a tool call; role-only and usage-only
// events do not count) give the first/last output times, and the usage
// event gives the token counts.
type sseLedgerReader struct {
	body     io.ReadCloser
	rec      *requestRecord
	line     []byte
	skipping bool
	now      func() time.Time
}

func newSSELedgerReader(body io.ReadCloser, rec *requestRecord) *sseLedgerReader {
	return &sseLedgerReader{body: body, rec: rec, now: time.Now}
}

func (s *sseLedgerReader) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		now := s.now()
		chunk := p[:n]
		for len(chunk) > 0 {
			i := bytes.IndexByte(chunk, '\n')
			if i < 0 {
				s.buffer(chunk)
				break
			}
			s.buffer(chunk[:i])
			s.event(now)
			chunk = chunk[i+1:]
		}
	}
	if errors.Is(err, io.EOF) {
		s.event(s.now())
	}
	return n, err
}

// Close is a no-op: proxy closes the original response body itself.
func (s *sseLedgerReader) Close() error { return nil }

func (s *sseLedgerReader) buffer(b []byte) {
	if s.skipping {
		return
	}
	if len(s.line)+len(b) > sseMaxLine {
		s.line, s.skipping = s.line[:0], true
		return
	}
	s.line = append(s.line, b...)
}

func (s *sseLedgerReader) event(now time.Time) {
	line := bytes.TrimSpace(s.line)
	s.line, s.skipping = s.line[:0], false
	payload, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return
	}
	payload = bytes.TrimSpace(payload)
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var ev struct {
		Model   string `json:"model"`
		Choices []struct {
			Text  string `json:"text"`
			Delta struct {
				Content          string          `json:"content"`
				ReasoningContent string          `json:"reasoning_content"`
				Reasoning        string          `json:"reasoning"`
				ToolCalls        json.RawMessage `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	for _, c := range ev.Choices {
		calls := string(c.Delta.ToolCalls)
		if c.Text != "" || c.Delta.Content != "" || c.Delta.ReasoningContent != "" || c.Delta.Reasoning != "" ||
			(calls != "" && calls != "null" && calls != "[]") {
			if s.rec.firstOutputAt.IsZero() {
				s.rec.firstOutputAt = now
			}
			s.rec.lastOutputAt = now
			break
		}
	}
	if ev.Usage != nil {
		s.rec.usage = &tokenUsage{model: ev.Model, prompt: ev.Usage.PromptTokens, cached: ev.Usage.PromptTokensDetails.CachedTokens, completion: ev.Usage.CompletionTokens}
	}
}
