// Usage accounting and the /platform usage panel's read API: real-currency
// pricing (as opposed to qos.Tracker's abstract alpha/beta cost units), the
// qos_events durable log those prices are written to, and the streaming
// usage-capture that feeds it. See docs/adr/0008-per-user-fair-share.md
// "Prometheus vs. ClickHouse": this deliberately reuses pat-db (Postgres)
// instead of the ClickHouse pipeline that ADR sketched -- a handful of
// developers' usage history doesn't warrant a second database and an
// airgap-registry image for it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"
)

// modelPrice is one model's price per 1000 tokens, in pricing.currency.
// CachedPer1K prices the cached portion of prompt_tokens (usage.go's
// promptTokensDetails.cached_tokens) separately from PromptPer1K, matching
// every real provider's prompt-caching discount -- a cache hit is cheaper to
// serve than a cold prefill, so it should be cheaper to bill.
type modelPrice struct {
	PromptPer1K     float64 `json:"prompt_per_1k"`
	CachedPer1K     float64 `json:"cached_per_1k"`
	CompletionPer1K float64 `json:"completion_per_1k"`
}

// pricing is the parsed contents of QOS_PRICING_FILE. The zero value (no
// file configured, or one that failed to load/parse) has an empty Models
// map, so cost() always returns 0 rather than erroring -- a missing price
// list degrades the usage panel to showing "-" for cost, it never blocks
// startup or the proxied request that would have priced it.
type pricing struct {
	Currency string                `json:"currency"`
	Models   map[string]modelPrice `json:"models"`
}

// defaultPriceKey is the fallback entry cost() uses for any model with no
// exact entry in pricing.json, so a new model doesn't silently cost 0 until
// someone remembers to price it.
const defaultPriceKey = "_default"

func loadPricing(path string) pricing {
	if path == "" {
		return pricing{}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf("qos pricing: read %s: %v", path, err)
		return pricing{}
	}
	var p pricing
	if err := json.Unmarshal(data, &p); err != nil {
		log.Printf("qos pricing: parse %s: %v", path, err)
		return pricing{}
	}
	return p
}

// cost prices promptTokens/cachedTokens/completionTokens at model's entry,
// or defaultPriceKey's when model has none, or 0 when neither exists.
// cachedTokens (a subset of promptTokens, per the OpenAI usage shape this
// service parses) is billed at CachedPer1K; the rest of promptTokens at
// PromptPer1K.
func (p pricing) cost(model string, promptTokens, cachedTokens, completionTokens int64) float64 {
	price, ok := p.Models[model]
	if !ok {
		if price, ok = p.Models[defaultPriceKey]; !ok {
			return 0
		}
	}
	if cachedTokens > promptTokens {
		cachedTokens = promptTokens
	}
	uncached := promptTokens - cachedTokens
	return price.PromptPer1K*float64(uncached)/1000 + price.CachedPer1K*float64(cachedTokens)/1000 + price.CompletionPer1K*float64(completionTokens)/1000
}

// ensureStreamUsage reports ok=false unchanged for any body that isn't a
// streaming chat-completion request, or that already asked for usage. For
// one that is, it returns body with `stream_options.include_usage: true`
// added, so vLLM/SGLang emit a final usage-bearing chunk that sseUsageReader
// can read -- the only way to price streamed chat traffic, which is most
// coding-agent traffic (docs/adr/0008-per-user-fair-share.md Stage 4's
// documented gap this closes). Round-tripping through map[string]any
// reorders and reformats the body, which is fine: this is the wire
// representation forwarded upstream, not something compared byte-for-byte
// anywhere.
func ensureStreamUsage(body []byte) ([]byte, bool) {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return nil, false
	}
	stream, _ := m["stream"].(bool)
	if !stream {
		return nil, false
	}
	opts, _ := m["stream_options"].(map[string]any)
	if already, ok := opts["include_usage"].(bool); ok && already {
		return nil, false
	}
	if opts == nil {
		opts = map[string]any{}
	}
	opts["include_usage"] = true
	m["stream_options"] = opts
	out, err := json.Marshal(m)
	if err != nil {
		return nil, false
	}
	return out, true
}

// sseUsageTailBytes bounds how much of a streamed chat-completion response
// sseUsageReader keeps: not the whole reply (which can run to hundreds of
// KB for a long agent turn), just enough trailing bytes to hold the last
// couple of SSE `data:` lines, since the usage-bearing chunk is always the
// final content event before `data: [DONE]`.
const sseUsageTailBytes = 8 << 10

// sseUsageReader passes every byte read from body through unmodified (so
// the client sees the exact same stream and timing it always has) while
// keeping a bounded tail of it. When body ends, it scans that tail for a
// usage object and calls onUsage once, if one was found. A client that
// disconnects before the upstream response itself ends can prevent that
// final Read/EOF from ever happening -- usage silently goes unrecorded for
// that one response, the same class of degradation every other observation
// point in this file accepts rather than adding retry/flush machinery for.
type sseUsageReader struct {
	body    io.ReadCloser
	tail    []byte
	done    bool
	onUsage func(model string, promptTokens, cachedTokens, completionTokens int64)
}

func newSSEUsageReader(body io.ReadCloser, onUsage func(string, int64, int64, int64)) *sseUsageReader {
	return &sseUsageReader{body: body, onUsage: onUsage}
}

func (s *sseUsageReader) Read(p []byte) (int, error) {
	n, err := s.body.Read(p)
	if n > 0 {
		s.tail = append(s.tail, p[:n]...)
		if len(s.tail) > sseUsageTailBytes {
			s.tail = append([]byte(nil), s.tail[len(s.tail)-sseUsageTailBytes:]...)
		}
	}
	if err != nil {
		s.finish()
	}
	return n, err
}

// Close is a no-op: main.go's proxy defers Close on the *original*
// resp.Body (captured before recordUsage reassigns it), so this is never
// actually called on the wrapper in practice, but it must exist to satisfy
// io.ReadCloser and must not double-close the shared underlying body.
func (s *sseUsageReader) Close() error { return nil }

func (s *sseUsageReader) finish() {
	if s.done {
		return
	}
	s.done = true
	if model, prompt, cached, completion, ok := parseSSEUsageTail(s.tail); ok {
		s.onUsage(model, prompt, cached, completion)
	}
}

var sseDataPrefix = []byte("data: ")
var sseDone = []byte("[DONE]")

// parseSSEUsageTail scans tail's lines for `data: {...}` chunks carrying a
// non-null "usage" object and returns the last one found -- last, not
// first, because only the terminal chunk of an include_usage stream carries
// it; every content chunk before it has "usage": null.
func parseSSEUsageTail(tail []byte) (model string, promptTokens, cachedTokens, completionTokens int64, ok bool) {
	for _, line := range bytes.Split(tail, []byte("\n")) {
		line = bytes.TrimSpace(line)
		payload, has := bytes.CutPrefix(line, sseDataPrefix)
		if !has || bytes.Equal(payload, sseDone) {
			continue
		}
		var parsed struct {
			Model string `json:"model"`
			Usage *struct {
				PromptTokens        int64 `json:"prompt_tokens"`
				CompletionTokens    int64 `json:"completion_tokens"`
				PromptTokensDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(payload, &parsed) != nil || parsed.Usage == nil {
			continue
		}
		model, promptTokens, cachedTokens, completionTokens, ok =
			parsed.Model, parsed.Usage.PromptTokens, parsed.Usage.PromptTokensDetails.CachedTokens, parsed.Usage.CompletionTokens, true
	}
	return
}

// recordQosEvent writes one qos_events row, priced from a.pricing. It never
// calls qos.RecordCost -- callers that need the scheduler's abstract cost
// unit updated call that separately (recordUsage does, for chat; not for
// embeddings, per docs/adr/0013-embeddings-api-bge-m3.md). A failed insert
// is logged and otherwise ignored: this always runs after the proxied
// response has already been delivered, so there is nothing left to degrade
// except the usage panel's own completeness.
func (a *app) recordQosEvent(ctx context.Context, owner, ownerName, tokenID, tokenName, sessionID, model, band string, promptTokens, cachedTokens, completionTokens int64) {
	if a.db == nil { // unit tests construct an app with no live pat-db
		return
	}
	cost := a.pricing.cost(model, promptTokens, cachedTokens, completionTokens)
	_, err := a.db.Exec(ctx, `INSERT INTO qos_events
		(owner_subject, owner_name, token_id, token_name, session_id, model, band, prompt_tokens, cached_tokens, completion_tokens, cost_amount)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		owner, ownerName, tokenID, tokenName, sessionID, model, band, promptTokens, cachedTokens, completionTokens, cost)
	if err != nil {
		log.Printf("qos event insert: %v", err)
	}
}

type dailyUsage struct {
	Date             string  `json:"date"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostAmount       float64 `json:"cost_amount"`
}

// usageDaily backs the /platform calendar heatmap: one row per UTC day with
// any activity in the last `days` days (default 90, capped 1-365).
func (a *app) usageDaily(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	days := clampIntQuery(r, "days", 90, 1, 365)
	rows, err := a.db.Query(r.Context(), `
		SELECT date_trunc('day', created_at AT TIME ZONE 'UTC') AS day,
		       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cost_amount),0)
		FROM qos_events
		WHERE owner_subject=$1 AND created_at >= now() - make_interval(days => $2)
		GROUP BY day ORDER BY day`, s.Subject, days)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	out := []dailyUsage{}
	for rows.Next() {
		var day time.Time
		var d dailyUsage
		if err := rows.Scan(&day, &d.PromptTokens, &d.CompletionTokens, &d.CostAmount); err != nil {
			http.Error(w, "database error", 500)
			return
		}
		d.Date = day.Format("2006-01-02")
		out = append(out, d)
	}
	writeJSON(w, 200, map[string]any{"currency": a.pricing.Currency, "days": out})
}

type sessionUsage struct {
	SessionID        string  `json:"session_id"`
	Model            string  `json:"model"`
	TokenName        string  `json:"token_name"`
	StartedAt        string  `json:"started_at"`
	EndedAt          string  `json:"ended_at"`
	Steps            int64   `json:"steps"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CostAmount       float64 `json:"cost_amount"`
}

// usageSessions backs the /platform session table: one row per session_id
// (excluding the empty session_id embeddings rows record), most recent
// first, paged by limit (default 50, capped 1-200) and offset.
func (a *app) usageSessions(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	limit := clampIntQuery(r, "limit", 50, 1, 200)
	offset := clampIntQuery(r, "offset", 0, 0, 1<<30)
	var total int64
	if err := a.db.QueryRow(r.Context(), `SELECT count(DISTINCT session_id) FROM qos_events WHERE owner_subject=$1 AND session_id <> ''`, s.Subject).Scan(&total); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	rows, err := a.db.Query(r.Context(), `
		SELECT session_id, max(model), max(token_name), min(created_at), max(created_at), count(*),
		       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cost_amount),0)
		FROM qos_events
		WHERE owner_subject=$1 AND session_id <> ''
		GROUP BY session_id ORDER BY max(created_at) DESC LIMIT $2 OFFSET $3`, s.Subject, limit, offset)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	out := []sessionUsage{}
	for rows.Next() {
		var started, ended time.Time
		var d sessionUsage
		if err := rows.Scan(&d.SessionID, &d.Model, &d.TokenName, &started, &ended, &d.Steps, &d.PromptTokens, &d.CompletionTokens, &d.CostAmount); err != nil {
			http.Error(w, "database error", 500)
			return
		}
		d.StartedAt, d.EndedAt = started.Format(time.RFC3339), ended.Format(time.RFC3339)
		out = append(out, d)
	}
	writeJSON(w, 200, map[string]any{"currency": a.pricing.Currency, "sessions": out, "total": total})
}

type usageWindow struct {
	Key    string  `json:"key"`
	Spent  float64 `json:"spent"`
	Tokens int64   `json:"tokens"`
}

// usageLimit backs the /platform usage widget: spend and tokens over the
// last hour, 24 hours, 7 days and this calendar month, plus this month's
// spend against QOS_MONTHLY_LIMIT. Read-only preview -- nothing here
// enforces the limit (see config.qosMonthlyLimit's doc comment).
func (a *app) usageLimit(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	now := time.Now().UTC()
	periodStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	periodEnd := periodStart.AddDate(0, 1, 0)
	windows := []usageWindow{{Key: "hour"}, {Key: "day"}, {Key: "week"}, {Key: "month"}}
	err := a.db.QueryRow(r.Context(), `
		SELECT COALESCE(SUM(cost_amount) FILTER (WHERE created_at >= now() - interval '1 hour'),0),
		       COALESCE(SUM(prompt_tokens+completion_tokens) FILTER (WHERE created_at >= now() - interval '1 hour'),0),
		       COALESCE(SUM(cost_amount) FILTER (WHERE created_at >= now() - interval '1 day'),0),
		       COALESCE(SUM(prompt_tokens+completion_tokens) FILTER (WHERE created_at >= now() - interval '1 day'),0),
		       COALESCE(SUM(cost_amount) FILTER (WHERE created_at >= now() - interval '7 days'),0),
		       COALESCE(SUM(prompt_tokens+completion_tokens) FILTER (WHERE created_at >= now() - interval '7 days'),0),
		       COALESCE(SUM(cost_amount) FILTER (WHERE created_at >= $2),0),
		       COALESCE(SUM(prompt_tokens+completion_tokens) FILTER (WHERE created_at >= $2),0)
		FROM qos_events
		WHERE owner_subject=$1 AND created_at >= LEAST($2, now() - interval '7 days')`,
		s.Subject, periodStart).Scan(&windows[0].Spent, &windows[0].Tokens, &windows[1].Spent, &windows[1].Tokens,
		&windows[2].Spent, &windows[2].Tokens, &windows[3].Spent, &windows[3].Tokens)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	writeJSON(w, 200, map[string]any{
		"currency":     a.pricing.Currency,
		"period_start": periodStart.Format("2006-01-02"),
		"period_end":   periodEnd.Format("2006-01-02"),
		"spent":        windows[3].Spent,
		"limit":        a.cfg.qosMonthlyLimit,
		"windows":      windows,
	})
}

// clampIntQuery reads an optional integer query param, clamped to [min,max],
// falling back to def when absent or unparsable.
func clampIntQuery(r *http.Request, name string, def, min, max int) int {
	v := r.URL.Query().Get(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
