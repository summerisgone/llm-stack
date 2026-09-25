package qos

import (
	"context"
	"log"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

// Metrics is the Stage 3 step 1 / Stage 4 subset needed to validate session
// stitching on real traffic: match quality and session counts. The fuller
// Stage 4 metric set (cost, queue, virtual clock, ...) is not implemented
// yet -- there is no admission logic to measure.
type Metrics struct {
	// SessionMatchTotal{result=matched|new|degraded} is the primary trust
	// signal for every other session-scoped number this package produces
	// (docs/adr/0008-per-user-fair-share.md "Known limitations").
	SessionMatchTotal *prometheus.CounterVec
	// SessionsStartedTotal{user} counts New outcomes.
	SessionsStartedTotal *prometheus.CounterVec
	// SessionStepsTotal{user} counts every Matched or New request, i.e.
	// every observed step for that user.
	SessionStepsTotal *prometheus.CounterVec
	// SessionsActive{user} is a gauge maintained by RunActiveSweep, not
	// updated inline -- it counts distinct session ids seen within the
	// last ttl per user, which requires periodically pruning stale
	// entries rather than an increment/decrement per request.
	SessionsActive *prometheus.GaugeVec
	// RotationsTotal{user} counts demotions: a session crossing
	// demoteAfterSteps consecutive steps, once per crossing (TASK-qos-fair-share.md
	// §4.2, Stage 5 test 7).
	RotationsTotal *prometheus.CounterVec
	// RequestsTotal{user,band,outcome} is Stage 4's activity counter for
	// chat-completion proxy calls. outcome: dispatched|rejected|error --
	// this package never sees "timeout" as a distinct case (that is
	// net/http's client timeout, indistinguishable here from any other Do
	// error).
	RequestsTotal *prometheus.CounterVec
	// PromptTokensTotal, CachedPromptTokensTotal and CompletionTokensTotal
	// {user,model} are TASK-qos-fair-share.md Stage 4's token-consumption
	// counters, fed by RecordCost from a completed response's usage. Only
	// populated for non-streaming responses today (main.go's recordUsage
	// does not parse streaming bodies) -- see ADR 0008 "Stage 4" for why
	// this is a deliberate, documented gap rather than an oversight.
	PromptTokensTotal       *prometheus.CounterVec
	CachedPromptTokensTotal *prometheus.CounterVec
	CompletionTokensTotal   *prometheus.CounterVec
	// CostUnitsTotal{user,model} is §4.4's cost formula accumulated over
	// time -- the GPU-cost axis a future QuotaPolicy would budget against,
	// same units RecordCost also feeds into per-user spend for band
	// modulation.
	CostUnitsTotal *prometheus.CounterVec
	// EmbeddingTokensTotal{user,model} is docs/adr/0013-embeddings-api-bge-m3.md's
	// accounting counter for /v1/embeddings, fed from recordUsage the same
	// way as PromptTokensTotal but never RecordCost: embedding tokens do not
	// consume the LLM's GPU slots or queue time, so they carry no cost unit.
	EmbeddingTokensTotal *prometheus.CounterVec
	// MCPCallsTotal{user,server,tool,status} counts /mcp/<server>/ requests
	// (docs/adr/0017-web-search-mcp-openserp-kagent.md section 3, docs/adr/0018
	// section 5). tool is
	// web_search|fetch_url|other for tools/call and "none" for the rest of
	// the MCP protocol (initialize, tools/list); status is the HTTP status.
	MCPCallsTotal *prometheus.CounterVec
}

// NewMetrics registers the Stage 3 metrics against reg and returns them.
// user is a label everywhere here because there are only a handful of
// developers sharing this GPU (TASK-qos-fair-share.md §1); session_id must
// never become a label (docs/adr/0008 "Prometheus vs. ClickHouse").
func NewMetrics(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		SessionMatchTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_session_match_total",
			Help: "Chain-hash session matches by outcome: matched, new, or degraded.",
		}, []string{"result"}),
		SessionsStartedTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_sessions_started_total",
			Help: "New sessions started per user.",
		}, []string{"user"}),
		SessionStepsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_session_steps_total",
			Help: "Requests matched to an existing or new session, per user.",
		}, []string{"user"}),
		SessionsActive: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "patsvc_sessions_active",
			Help: "Distinct sessions seen within the session TTL window, per user.",
		}, []string{"user"}),
		RotationsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_rotations_total",
			Help: "Sessions demoted after running demoteAfterSteps consecutive steps, per user.",
		}, []string{"user"}),
		RequestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_requests_total",
			Help: "Chat-completion proxy calls by outcome: dispatched, rejected, or error.",
		}, []string{"user", "band", "outcome"}),
		PromptTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_prompt_tokens_total",
			Help: "Prompt tokens consumed, per user and model, from completed non-streaming responses.",
		}, []string{"user", "model"}),
		CachedPromptTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_cached_prompt_tokens_total",
			Help: "Prompt tokens served from cache, per user and model. Zero on engines that never populate usage.prompt_tokens_details.cached_tokens (ADR 0008 \"Verifying the prediction\").",
		}, []string{"user", "model"}),
		CompletionTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_completion_tokens_total",
			Help: "Completion tokens generated, per user and model, from completed non-streaming responses.",
		}, []string{"user", "model"}),
		CostUnitsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_cost_units_total",
			Help: "TASK-qos-fair-share.md §4.4 cost units consumed (alpha*uncached_prompt + beta*completion), per user and model.",
		}, []string{"user", "model"}),
		EmbeddingTokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_embedding_tokens_total",
			Help: "Prompt tokens consumed by /v1/embeddings, per user and model, from completed non-streaming responses.",
		}, []string{"user", "model"}),
		MCPCallsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "patsvc_mcp_calls_total",
			Help: "MCP requests proxied to MCP servers, per user, server, tool and HTTP status.",
		}, []string{"user", "server", "tool", "status"}),
	}
	reg.MustRegister(m.SessionMatchTotal, m.SessionsStartedTotal, m.SessionStepsTotal, m.SessionsActive, m.RotationsTotal,
		m.RequestsTotal, m.PromptTokensTotal, m.CachedPromptTokensTotal, m.CompletionTokensTotal, m.CostUnitsTotal,
		m.EmbeddingTokensTotal, m.MCPCallsTotal)
	return m
}

// ActiveStore is the subset of Store the active-session gauge sweep needs.
// Split from Store so RedisStore's active-set bookkeeping stays next to the
// gauge logic that consumes it.
type ActiveStore interface {
	ActiveUsers(ctx context.Context) ([]string, error)
	ActiveSessionCount(ctx context.Context, sub string, cutoff time.Time) (int64, error)
}

// RunActiveSweep periodically prunes each known user's active-session set
// to entries seen within ttl and republishes SessionsActive. It blocks until
// ctx is cancelled; run it in its own goroutine. A sweep error (e.g. Valkey
// briefly unreachable) is logged and skipped -- the gauge simply keeps its
// last value until the next tick, never blocking request handling.
func (m *Metrics) RunActiveSweep(ctx context.Context, store ActiveStore, ttl time.Duration, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.sweepOnce(ctx, store, ttl)
		}
	}
}

func (m *Metrics) sweepOnce(ctx context.Context, store ActiveStore, ttl time.Duration) {
	users, err := store.ActiveUsers(ctx)
	if err != nil {
		log.Printf("qos active-session sweep: list users: %v", err)
		return
	}
	cutoff := time.Now().Add(-ttl)
	for _, user := range users {
		count, err := store.ActiveSessionCount(ctx, user, cutoff)
		if err != nil {
			log.Printf("qos active-session sweep: user=%s: %v", user, err)
			continue
		}
		m.SessionsActive.WithLabelValues(user).Set(float64(count))
	}
}
