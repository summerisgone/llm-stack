package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// chunkedBody returns each part from a separate Read, the way a stream
// arrives over the network.
type chunkedBody struct{ parts []string }

func (c *chunkedBody) Read(p []byte) (int, error) {
	if len(c.parts) == 0 {
		return 0, io.EOF
	}
	n := copy(p, c.parts[0])
	c.parts[0] = c.parts[0][n:]
	if c.parts[0] == "" {
		c.parts = c.parts[1:]
	}
	return n, nil
}
func (c *chunkedBody) Close() error { return nil }

func TestSSELedgerReaderTimesMeaningfulOutputOnly(t *testing.T) {
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	parts := []string{
		"data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",           // t+1 role only
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thin",             // t+2 split mid-line
		"king\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n", // t+3
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0}]}}]}\n\n",   // t+4
		"data: {\"choices\":[{\"delta\":{\"content\":\"\"},\"finish_reason\":\"stop\"}]}\n\n" + // t+5 empty
			"data: {\"model\":\"qwen\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":40}}}\n\n" +
			"data: [DONE]\n\n",
	}
	want := strings.Join(parts, "")
	rec := &requestRecord{}
	reader := newSSELedgerReader(&chunkedBody{parts: append([]string(nil), parts...)}, rec)
	tick := 0
	reader.now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != want {
		t.Fatalf("body altered: %q, %v", got, err)
	}
	if !rec.firstOutputAt.Equal(base.Add(3*time.Second)) || !rec.lastOutputAt.Equal(base.Add(4*time.Second)) {
		t.Errorf("first %v last %v, want t+3 (split reasoning completes) and t+4 (tool call)", rec.firstOutputAt, rec.lastOutputAt)
	}
	if u := rec.usage; u == nil || u.model != "qwen" || u.prompt != 100 || u.cached != 40 || u.completion != 7 {
		t.Errorf("usage = %+v", rec.usage)
	}
}

func TestSSELedgerReaderWithoutUsageOrOutput(t *testing.T) {
	rec := &requestRecord{}
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]"
	got, _ := io.ReadAll(newSSELedgerReader(io.NopCloser(strings.NewReader(body)), rec))
	if string(got) != body || rec.usage != nil || !rec.firstOutputAt.IsZero() {
		t.Fatalf("rec = %+v", rec)
	}
}

func TestSSELedgerReaderSkipsOverlongLines(t *testing.T) {
	rec := &requestRecord{}
	huge := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("x", sseMaxLine) + "\"}}]}\n\n"
	tail := "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2}}\n\n"
	got, _ := io.ReadAll(newSSELedgerReader(io.NopCloser(strings.NewReader(huge+tail)), rec))
	if len(got) != len(huge+tail) || rec.usage == nil || rec.usage.completion != 2 {
		t.Fatalf("len %d, usage %+v", len(got), rec.usage)
	}
}

func TestRequestOutcome(t *testing.T) {
	live := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	gone := live.WithContext(ctx)
	for _, tc := range []struct {
		r      *http.Request
		status int
		err    error
		want   string
	}{
		{live, 200, nil, "ok"},
		{live, 0, errors.New("dial"), "error"},
		{live, 502, nil, "error"},
		{live, 429, nil, "rejected"},
		{live, 400, nil, "client_error"},
		{gone, 200, context.Canceled, "cancelled"},
		{gone, 0, context.Canceled, "cancelled"}, // client left before response headers
		{live, 200, errors.New("upstream reset"), "error"},
	} {
		if got := requestOutcome(tc.r, &requestRecord{status: tc.status}, tc.err); got != tc.want {
			t.Errorf("status %d err %v: %s, want %s", tc.status, tc.err, got, tc.want)
		}
	}
}

// --- end to end through proxy, with Postgres (PATDB_TEST_DSN) ---

type ledgerRow struct {
	source, outcome, quality, model, session, band string
	status                                         *int
	streaming                                      *bool
	prompt, completion                             int64
	received, dispatched, first, last, finished    *time.Time
}

func proxyTestApp(t *testing.T, upstream http.HandlerFunc) (*app, string) {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)
	a := testApp()
	a.db = testDB(t)
	a.cfg.hashKey = []byte(strings.Repeat("h", 32))
	a.cfg.issuer = testIssuer
	a.cfg.gatewayURL = srv.URL
	a.cfg.qosEventTimeout = time.Second
	a.proxyHTTP = srv.Client()
	a.gateway = gatewayToken{value: "gw", expires: time.Now().Add(time.Hour)}
	ctx := context.Background()
	if err := a.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	token := "sk-ledger-test"
	if _, err := a.db.Exec(ctx, `INSERT INTO personal_access_tokens (id, owner_issuer, owner_subject, owner_name, token_hash, token_prefix, name, expires_at, issued_by)
		VALUES ('tok', $1, 'alice', 'alice', $2, 'sk-ledger', 'agent', now() + interval '1 day', 'agents')`, testIssuer, a.tokenHash(token)); err != nil {
		t.Fatal(err)
	}
	return a, token
}

func lastLedgerRow(t *testing.T, a *app) ledgerRow {
	t.Helper()
	var row ledgerRow
	if err := a.db.QueryRow(context.Background(), `SELECT source, outcome, usage_quality, model, session_id, band, http_status, streaming,
		prompt_tokens, completion_tokens, received_at, dispatched_at, first_output_at, last_output_at, finished_at
		FROM qos_events ORDER BY id DESC LIMIT 1`).Scan(&row.source, &row.outcome, &row.quality, &row.model, &row.session, &row.band,
		&row.status, &row.streaming, &row.prompt, &row.completion, &row.received, &row.dispatched, &row.first, &row.last, &row.finished); err != nil {
		t.Fatal(err)
	}
	return row
}

func TestProxyWritesLedgerForStreamingChat(t *testing.T) {
	a, token := proxyTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flush := w.(http.Flusher)
		for _, ev := range []string{`{"choices":[{"delta":{"role":"assistant"}}]}`, `{"choices":[{"delta":{"content":"a"}}]}`, `{"choices":[{"delta":{"content":"b"}}]}`,
			`{"model":"qwen","choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2}}`} {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			flush.Flush()
			time.Sleep(20 * time.Millisecond)
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	a.proxy(rec, r)
	if rec.Code != 200 {
		t.Fatalf("proxy: %d %s", rec.Code, rec.Body)
	}
	row := lastLedgerRow(t, a)
	if row.source != "agent" || row.outcome != "ok" || row.quality != "actual" || row.model != "qwen" || row.prompt != 10 || row.completion != 2 || *row.status != 200 || !*row.streaming {
		t.Fatalf("row = %+v", row)
	}
	if row.first == nil || row.last == nil || row.received == nil || row.dispatched == nil || row.finished == nil {
		t.Fatalf("missing timestamps: %+v", row)
	}
	if row.received.After(*row.dispatched) || row.dispatched.After(*row.first) || !row.first.Before(*row.last) || row.last.After(*row.finished) {
		t.Errorf("timestamps out of order: received %v dispatched %v first %v last %v finished %v", row.received, row.dispatched, row.first, row.last, row.finished)
	}
}

func TestProxyWritesLedgerForFailures(t *testing.T) {
	status := http.StatusTooManyRequests
	a, token := proxyTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"error":"busy"}`)
	})
	call := func(path string) {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"model":"bge-m3","input":"x"}`))
		r.Header.Set("Authorization", "Bearer "+token)
		a.proxy(httptest.NewRecorder(), r)
	}
	call("/v1/chat/completions")
	if row := lastLedgerRow(t, a); row.outcome != "rejected" || row.quality != "unknown" || row.prompt != 0 || row.first != nil {
		t.Errorf("429 row = %+v", row)
	}
	status = http.StatusOK
	call("/v1/models")
	if row := lastLedgerRow(t, a); row.outcome != "rejected" {
		t.Errorf("/v1/models must not be recorded, last row = %+v", row)
	}
	a.cfg.gatewayURL = "http://127.0.0.1:1"
	call("/v1/embeddings")
	if row := lastLedgerRow(t, a); row.outcome != "error" || row.status != nil || row.band != "embeddings" || row.session != "" {
		t.Errorf("transport failure row = %+v", row)
	}
}

func TestProxyWritesLedgerWhenClientCancels(t *testing.T) {
	release := make(chan struct{})
	a, token := proxyTestApp(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		w.(http.Flusher).Flush()
		<-release
	})
	defer close(release)
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"qwen","stream":true,"messages":[]}`)).WithContext(ctx)
	r.Header.Set("Authorization", "Bearer "+token)
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	a.proxy(httptest.NewRecorder(), r)
	row := lastLedgerRow(t, a)
	if row.outcome != "cancelled" || row.quality != "unknown" || row.first == nil {
		t.Fatalf("row = %+v", row)
	}
}
