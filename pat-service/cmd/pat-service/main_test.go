package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/airgap-ai-stack/pat-service/internal/qos"
)

// testApp returns an app whose QoS tracker is wired to an unreachable
// Valkey address, so recordUsage exercises its real parsing path but every
// store call fails open (no live Redis needed for these HTTP-plumbing
// tests -- qos package's own tests cover the store-backed decision logic).
func testApp() *app {
	store := qos.NewRedisStore("127.0.0.1:1")
	tracker := qos.NewTracker(store, qos.NewMetrics(prometheus.NewRegistry()), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	return &app{
		cfg: config{qosUsageMaxBody: 1 << 20, qosFingerprintTimeout: 50 * time.Millisecond, qosEventTimeout: 50 * time.Millisecond},
		qos: tracker,
	}
}

func TestMuxRegistersDashboardAndOpenAIPathsWithoutConflict(t *testing.T) {
	mux := newMux(&app{})

	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		req := httptest.NewRequest(method, "/v1/models", nil)
		_, pattern := mux.Handler(req)
		if pattern != method+" /v1/" {
			t.Fatalf("%s /v1/models matched %q, want %q", method, pattern, method+" /v1/")
		}
	}

	_, pattern := mux.Handler(httptest.NewRequest(http.MethodGet, "/", nil))
	if pattern != "GET /" {
		t.Fatalf("GET / matched %q, want GET /", pattern)
	}
}

func TestDashboardBuildIsEmbedded(t *testing.T) {
	page, err := dashboardAssets.ReadFile("assets/index.html")
	if err != nil {
		t.Fatalf("read embedded dashboard: %v", err)
	}
	if !bytes.Contains(page, []byte("Personal access tokens")) {
		t.Fatal("embedded dashboard does not contain the application shell")
	}
}

func TestEmbeddedDashboardAssetIsServed(t *testing.T) {
	entries, err := dashboardAssets.ReadDir("assets/assets")
	if err != nil {
		t.Fatalf("read asset directory: %v", err)
	}
	assetName := ""
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".js") {
			assetName = entry.Name()
			break
		}
	}
	if assetName == "" {
		t.Fatal("no JavaScript dashboard asset is embedded")
	}

	response := httptest.NewRecorder()
	newMux(&app{}).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/assets/assets/"+assetName, nil))
	if response.Code != http.StatusOK {
		t.Fatalf("asset returned %d, want 200", response.Code)
	}
}

func TestCopyRequestHeadersStripsLLMDFairnessControls(t *testing.T) {
	src := http.Header{
		"X-Llm-D-Inference-Fairness-Id": {"spoofed-tenant"},
		"X-Llm-D-Inference-Objective":   {"premium"},
		"X-User-Name":                   {"spoofed-user"},
		"X-Session-Key":                 {"spoofed-session"},
		"Agent-Session-Id":              {"spoofed-agent-session"},
		"X-Request-Id":                  {"request-1"},
	}
	dst := make(http.Header)

	copyRequestHeaders(dst, src)

	if dst.Get("X-Llm-D-Inference-Fairness-Id") != "" {
		t.Fatal("client supplied llm-d fairness id was forwarded")
	}
	if dst.Get("X-User-Name") != "" {
		t.Fatal("client supplied user name was forwarded")
	}
	if dst.Get("X-Llm-D-Inference-Objective") != "" {
		t.Fatal("client supplied llm-d objective was forwarded")
	}
	if dst.Get("X-Session-Key") != "" {
		t.Fatal("client supplied session key was forwarded")
	}
	if dst.Get("Agent-Session-Id") != "" {
		t.Fatal("client supplied agent session id was forwarded")
	}
	if got := dst.Get("X-Request-Id"); got != "request-1" {
		t.Fatalf("ordinary request header = %q, want request-1", got)
	}
}

func TestRecordUsageIgnoresNonChatPaths(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	body := `{"data":[]}`
	resp := &http.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}

	got, err := io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "m", qos.BandNormal))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q unchanged for a non-chat path", got, body)
	}
}

func TestRecordUsageNeverBuffersAStreamingResponse(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := "data: {\"choices\":[]}\n\ndata: [DONE]\n\n"
	resp := &http.Response{Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}

	got, err := io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "m", qos.BandNormal))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("streamed body = %q, want %q unchanged", got, body)
	}
}

func TestRecordUsagePreservesNonStreamingChatCompletionBody(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := `{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":20}}}`
	resp := &http.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}

	got, err := io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "m", qos.BandNormal))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q unchanged after cost extraction", got, body)
	}
}

func TestRecordUsagePassesThroughOversizedResponses(t *testing.T) {
	a := testApp()
	a.cfg.qosUsageMaxBody = 4
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := `{"usage":{"prompt_tokens":1}}`
	resp := &http.Response{Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}

	got, err := io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "m", qos.BandNormal))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q unchanged when over qosUsageMaxBody", got, body)
	}
}

func TestRecordUsageIncrementsRequestsTotalByOutcome(t *testing.T) {
	cases := []struct {
		status  int
		outcome string
	}{
		{http.StatusOK, "dispatched"},
		{http.StatusTooManyRequests, "rejected"},
		{http.StatusBadGateway, "error"},
	}
	for _, tc := range cases {
		a := testApp()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}

		_, _ = io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "m", qos.BandWarm))

		if got := testutil.ToFloat64(a.qos.Metrics().RequestsTotal.WithLabelValues("alice", "warm", tc.outcome)); got != 1 {
			t.Fatalf("status %d: patsvc_requests_total{outcome=%s} = %v, want 1", tc.status, tc.outcome, got)
		}
	}
}

func TestRecordUsageIgnoresNonChatPathsForRequestsTotal(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{}`))}

	_, _ = io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "m", qos.BandNormal))

	if got := testutil.ToFloat64(a.qos.Metrics().RequestsTotal.WithLabelValues("alice", "normal", "dispatched")); got != 0 {
		t.Fatalf("patsvc_requests_total for a non-chat path = %v, want 0", got)
	}
}

func TestRecordUsageFeedsTokenAndCostCounters(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := `{"usage":{"prompt_tokens":100,"completion_tokens":10,"prompt_tokens_details":{"cached_tokens":20}}}`
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}

	_, _ = io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "qwen-3.8-27b", qos.BandNormal))

	metrics := a.qos.Metrics()
	if got := testutil.ToFloat64(metrics.PromptTokensTotal.WithLabelValues("alice", "qwen-3.8-27b")); got != 100 {
		t.Fatalf("patsvc_prompt_tokens_total = %v, want 100", got)
	}
	if got := testutil.ToFloat64(metrics.CachedPromptTokensTotal.WithLabelValues("alice", "qwen-3.8-27b")); got != 20 {
		t.Fatalf("patsvc_cached_prompt_tokens_total = %v, want 20", got)
	}
	if got := testutil.ToFloat64(metrics.CompletionTokensTotal.WithLabelValues("alice", "qwen-3.8-27b")); got != 10 {
		t.Fatalf("patsvc_completion_tokens_total = %v, want 10", got)
	}
	if got := testutil.ToFloat64(metrics.CostUnitsTotal.WithLabelValues("alice", "qwen-3.8-27b")); got <= 0 {
		t.Fatalf("patsvc_cost_units_total = %v, want > 0", got)
	}
}

// docs/adr/0013-embeddings-api-bge-m3.md: /v1/embeddings gets its own
// accounting path through recordUsage, distinct from chat.

func TestRecordUsageEmbeddingsIncrementsRequestsTotalWithEmbeddingsBand(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil)
	body := `{"model":"bge-m3","data":[{"embedding":[0.1]}],"usage":{"prompt_tokens":42}}`
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}

	got, err := io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "bge-m3", qos.BandNormal))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != body {
		t.Fatalf("body = %q, want %q unchanged after usage extraction", got, body)
	}
	if got := testutil.ToFloat64(a.qos.Metrics().RequestsTotal.WithLabelValues("alice", "embeddings", "dispatched")); got != 1 {
		t.Fatalf("patsvc_requests_total{band=embeddings} = %v, want 1", got)
	}
}

func TestRecordUsageEmbeddingsFeedsEmbeddingTokensNotCost(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest(http.MethodPost, "/v1/embeddings", nil)
	body := `{"model":"bge-m3","usage":{"prompt_tokens":42}}`
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}

	_, _ = io.ReadAll(a.recordUsage(r, resp, "alice", "alice", "tok-1", "test token", "", "bge-m3", qos.BandNormal))

	metrics := a.qos.Metrics()
	if got := testutil.ToFloat64(metrics.EmbeddingTokensTotal.WithLabelValues("alice", "bge-m3")); got != 42 {
		t.Fatalf("patsvc_embedding_tokens_total = %v, want 42", got)
	}
	if got := testutil.ToFloat64(metrics.CostUnitsTotal.WithLabelValues("alice", "bge-m3")); got != 0 {
		t.Fatalf("patsvc_cost_units_total for an embeddings request = %v, want 0 (embeddings never feeds RecordCost)", got)
	}
}
