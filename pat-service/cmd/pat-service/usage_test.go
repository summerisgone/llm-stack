package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/airgap-ai-stack/pat-service/internal/qos"
)

func TestEnsureStreamUsageInjectsIncludeUsage(t *testing.T) {
	out, ok := ensureStreamUsage([]byte(`{"model":"m","stream":true,"messages":[]}`))
	if !ok {
		t.Fatal("ok = false, want true for a streaming request with no stream_options")
	}
	if !strings.Contains(string(out), `"include_usage":true`) {
		t.Fatalf("mutated body = %s, want it to carry stream_options.include_usage", out)
	}
}

func TestEnsureStreamUsageLeavesNonStreamingRequestsAlone(t *testing.T) {
	if _, ok := ensureStreamUsage([]byte(`{"model":"m","messages":[]}`)); ok {
		t.Fatal("ok = true for a non-streaming request, want false (untouched)")
	}
}

func TestEnsureStreamUsageIsIdempotent(t *testing.T) {
	if _, ok := ensureStreamUsage([]byte(`{"stream":true,"stream_options":{"include_usage":true}}`)); ok {
		t.Fatal("ok = true when include_usage was already set, want false (no rewrite needed)")
	}
}

func TestEnsureStreamUsageDegradesOnUnparsableBody(t *testing.T) {
	if _, ok := ensureStreamUsage([]byte(`not json`)); ok {
		t.Fatal("ok = true for unparsable JSON, want false")
	}
}

func TestRecordUsageStreamingFeedsTokenAndCostCounters(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	body := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":null}\n\n" +
		"data: {\"model\":\"qwen-3.8-27b\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":10,\"prompt_tokens_details\":{\"cached_tokens\":20}}}\n\n" +
		"data: [DONE]\n\n"
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(body))}

	if got := deliver(a, r, resp, testRecord(r, "sess-1", "unknown", qos.BandNormal)); got != body {
		t.Fatalf("streamed body = %q, want %q unchanged", got, body)
	}

	metrics := a.qos.Metrics()
	if got := testutil.ToFloat64(metrics.PromptTokensTotal.WithLabelValues("alice", "qwen-3.8-27b")); got != 100 {
		t.Fatalf("patsvc_prompt_tokens_total = %v, want 100 (streaming usage should feed the same counters as non-streaming)", got)
	}
	if got := testutil.ToFloat64(metrics.CostUnitsTotal.WithLabelValues("alice", "qwen-3.8-27b")); got <= 0 {
		t.Fatalf("patsvc_cost_units_total = %v, want > 0", got)
	}
}

func TestPricingCostFallsBackToDefault(t *testing.T) {
	p := pricing{Currency: "RUB", Models: map[string]modelPrice{
		"_default": {PromptPer1K: 1, CachedPer1K: 1, CompletionPer1K: 3},
	}}
	if got := p.cost("some-unpriced-model", 1000, 0, 1000); got != 4 {
		t.Fatalf("cost = %v, want 4 (1 + 3 per 1K via _default)", got)
	}
}

func TestPricingCostUsesExactModelMatch(t *testing.T) {
	p := pricing{Models: map[string]modelPrice{
		"qwen":     {PromptPer1K: 2, CompletionPer1K: 5},
		"_default": {PromptPer1K: 1, CompletionPer1K: 1},
	}}
	if got := p.cost("qwen", 1000, 0, 0); got != 2 {
		t.Fatalf("cost = %v, want 2 (exact model entry, not _default)", got)
	}
}

func TestPricingCostZeroWhenUnconfigured(t *testing.T) {
	var p pricing
	if got := p.cost("anything", 1000, 0, 1000); got != 0 {
		t.Fatalf("cost = %v, want 0 for empty pricing (no file loaded)", got)
	}
}

func TestPricingCostBillsCachedTokensAtTheCacheRate(t *testing.T) {
	p := pricing{Models: map[string]modelPrice{
		"qwen": {PromptPer1K: 1, CachedPer1K: 0.1, CompletionPer1K: 0},
	}}
	// 1000 prompt tokens, 600 of them cached: 400 uncached @1 + 600 cached @0.1.
	if got := p.cost("qwen", 1000, 600, 0); got != 0.46 {
		t.Fatalf("cost = %v, want 0.46 (400*1/1000 + 600*0.1/1000)", got)
	}
}

func TestPricingCostClampsCachedTokensToPromptTokens(t *testing.T) {
	p := pricing{Models: map[string]modelPrice{
		"qwen": {PromptPer1K: 1, CachedPer1K: 0.1, CompletionPer1K: 0},
	}}
	// A malformed usage block reporting more cached than total prompt
	// tokens must not go negative on the uncached share.
	if got := p.cost("qwen", 100, 500, 0); got != 0.01 {
		t.Fatalf("cost = %v, want 0.01 (100 tokens all billed at the cache rate: 100*0.1/1000)", got)
	}
}

func TestPricingCostQwen38_27BMatchesConfiguredChargebackRate(t *testing.T) {
	// k8s/base/applications.yaml's pat-service-pricing ConfigMap: RadixArk-
	// Qwen3.8-27B-NVFP4 is self-hosted with no vendor API, so these are a
	// team-set chargeback rate ($0.10/$2.475/$0.0495 per 1M in/out/cached),
	// not a fetched market price. This test pins the ConfigMap's numbers so
	// a future edit there is deliberate, not a typo.
	p := pricing{Models: map[string]modelPrice{
		"qwen-3.8-27b": {PromptPer1K: 0.0001, CachedPer1K: 0.0000495, CompletionPer1K: 0.002475},
	}}
	got := p.cost("qwen-3.8-27b", 1_000_000, 0, 1_000_000)
	want := 0.10 + 2.475
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("cost = %v, want %v (1M prompt + 1M completion tokens at the per-1M rates)", got, want)
	}
}
