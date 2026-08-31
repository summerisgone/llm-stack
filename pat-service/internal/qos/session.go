package qos

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Store is the Valkey-backed state the chain match and band assignment
// need. It is an interface so this logic can be unit tested against an
// in-memory fake instead of a live Valkey.
type Store interface {
	// GetSessionForChain returns the session id recorded for this chain
	// link under this user, or "" if there is none.
	GetSessionForChain(ctx context.Context, sub, link string) (string, error)
	// SetSessionForChain records/refreshes a chain link -> session id
	// mapping with a sliding TTL.
	SetSessionForChain(ctx context.Context, sub, link, sessionID string, ttl time.Duration) error
	// IncrStep increments and returns the session's step counter,
	// refreshing its TTL.
	IncrStep(ctx context.Context, sub, sessionID string, ttl time.Duration) (int64, error)
	// TouchActive records sessionID as active for sub as of now, for the
	// active-session gauge sweep.
	TouchActive(ctx context.Context, sub, sessionID string, now time.Time) error
	// BumpStreak advances sub's consecutive-step counter for sessionID:
	// it resets to 1 when sessionID differs from the last one seen for
	// sub, otherwise increments. This is TASK-qos-fair-share.md §4.2's
	// "N шагов одной сессии подряд" -- the demotion trigger -- and is
	// deliberately separate from IncrStep's per-session lifetime total.
	BumpStreak(ctx context.Context, sub, sessionID string, ttl time.Duration) (streak int64, err error)
	// TouchDispatch records sessionID as dispatched for sub now and
	// returns how long it had been since the previous dispatch (hadPrev
	// is false on the session's first recorded dispatch). This is the
	// WARM_TTL clock for §4.2's warm band.
	TouchDispatch(ctx context.Context, sub, sessionID string, now time.Time, ttl time.Duration) (sinceLast time.Duration, hadPrev bool, err error)
	// GetSpend returns sub's recent cumulative cost (§4.4), or 0 if sub
	// has no recorded spend (never spent, or its window expired).
	GetSpend(ctx context.Context, sub string) (float64, error)
	// AddSpend adds cost to sub's recent cumulative spend and refreshes
	// its window, returning the new total. The window is a TTL refreshed
	// on every add, not a true sliding window: spend resets to zero only
	// once sub goes fully idle for longer than window, which is the
	// intended behaviour -- recent spend, not lifetime spend, is what
	// should drive demotion.
	AddSpend(ctx context.Context, sub string, cost float64, window time.Duration) (total float64, err error)
	// MinActiveSpend returns the lowest recent spend among users other
	// than excludeSub that are currently active (i.e. have dispatched a
	// request within the session TTL, per TouchActive), and whether any
	// such user exists. A lone active user has no one to be "ahead of."
	MinActiveSpend(ctx context.Context, excludeSub string) (min float64, hasOthers bool, err error)
}

// Band is TASK-qos-fair-share.md §4.1's InferenceObjective name: the
// per-request priority band llm-d's EPP resolves via the
// x-llm-d-inference-objective header (ADR 0008 "Stage 1B").
type Band string

const (
	BandWarm    Band = "warm"
	BandNormal  Band = "normal"
	BandDemoted Band = "demoted"
)

// Outcome is the result label for patsvc_session_match_total.
type Outcome string

const (
	// Matched means a chain link matched a previously recorded session.
	Matched Outcome = "matched"
	// New means no chain link matched; a new session id was minted.
	New Outcome = "new"
	// Degraded means fingerprinting was skipped or failed (non-chat
	// request, oversized body, malformed JSON, or the store errored) --
	// per TASK-qos-fair-share.md Stage 3's "degradation, not a failure"
	// rule, this never affects the proxied request.
	Degraded Outcome = "degraded"
)

// Result is what one Observe call found. Band is the only field that feeds
// a decision (the InferenceObjective header); the rest is for
// logging/attribution.
type Result struct {
	SessionID string
	Step      int64
	Outcome   Outcome
	Band      Band
	// Model is the request's declared model name, for Stage 4 metric
	// labels only ("" on a Degraded outcome -- the caller defaults it).
	Model string
}

// Tracker computes the chain match for a request body, records it, and
// assigns the request's priority band. The queue itself -- which band goes
// next, how many requests run concurrently -- is llm-d's, not this
// package's (TASK-qos-fair-share.md §2.1); Tracker only ever produces a
// header value, never blocks or reorders a request.
type Tracker struct {
	store                Store
	metrics              *Metrics
	ttl                  time.Duration
	warmTTL              time.Duration
	demoteAfterSteps     int64
	costAlpha            float64
	costBeta             float64
	spendWindow          time.Duration
	spendDemoteThreshold float64
}

// NewTracker builds a Tracker. ttl is the sliding TTL applied to chain
// links and the step counter (TASK-qos-fair-share.md §5.3 default: 30
// minutes). warmTTL and demoteAfterSteps are §4.2's band thresholds
// (defaults: 120s, 8 consecutive steps). costAlpha/costBeta are §4.4's
// per-token cost coefficients; spendWindow and spendDemoteThreshold control
// the cost-based modulation described there ("пользователь, ушедший далеко
// вперёд по расходу, временно получает полосу ниже").
func NewTracker(store Store, metrics *Metrics, ttl, warmTTL time.Duration, demoteAfterSteps int64, costAlpha, costBeta, spendDemoteThreshold float64, spendWindow time.Duration) *Tracker {
	return &Tracker{
		store:                store,
		metrics:              metrics,
		ttl:                  ttl,
		warmTTL:              warmTTL,
		demoteAfterSteps:     demoteAfterSteps,
		costAlpha:            costAlpha,
		costBeta:             costBeta,
		spendWindow:          spendWindow,
		spendDemoteThreshold: spendDemoteThreshold,
	}
}

// Observe matches body against sub's recorded session chains, records the
// match, and returns the outcome. It never returns an error: any failure
// (bad JSON, non-chat request, store unavailable) degrades to Degraded so
// the caller can always proceed with the proxied request unaffected -- this
// is the "не сломай ... деградация, а не отказ" rule from
// TASK-qos-fair-share.md's Stage 3 preamble, generalized from body-size
// overflow to every failure mode here.
func (t *Tracker) Observe(ctx context.Context, sub string, body []byte) Result {
	req, ok := parseChatRequest(body)
	if !ok {
		t.metrics.SessionMatchTotal.WithLabelValues(string(Degraded)).Inc()
		return Result{Outcome: Degraded}
	}
	links, err := chain(req)
	if err != nil || len(links) == 0 {
		t.metrics.SessionMatchTotal.WithLabelValues(string(Degraded)).Inc()
		return Result{Outcome: Degraded}
	}

	sessionID := ""
	for i := len(links) - 1; i >= 0; i-- {
		sid, err := t.store.GetSessionForChain(ctx, sub, links[i])
		if err != nil {
			// Valkey unavailable or slow: fail open, record degraded, do
			// not touch the request.
			t.metrics.SessionMatchTotal.WithLabelValues(string(Degraded)).Inc()
			return Result{Outcome: Degraded}
		}
		if sid != "" {
			sessionID = sid
			break
		}
	}

	outcome := Matched
	if sessionID == "" {
		outcome = New
		sessionID = newSessionID()
	}

	// Refresh every link in this request's chain to the matched (or new)
	// session id. Each link's own TTL bounds per-session Valkey memory: a
	// session with no traffic for longer than ttl loses its links and
	// looks new on the next request, which is the correct behaviour, not
	// a bug (TASK-qos-fair-share.md §3.3: "absence of a match is not a
	// failure, it's a correctly identified new session").
	for _, link := range links {
		_ = t.store.SetSessionForChain(ctx, sub, link, sessionID, t.ttl)
	}

	step, err := t.store.IncrStep(ctx, sub, sessionID, t.ttl)
	if err != nil {
		step = 0
	}
	_ = t.store.TouchActive(ctx, sub, sessionID, time.Now())
	band := t.assignBand(ctx, sub, sessionID)

	t.metrics.SessionMatchTotal.WithLabelValues(string(outcome)).Inc()
	t.metrics.SessionStepsTotal.WithLabelValues(sub).Inc()
	if outcome == New {
		t.metrics.SessionsStartedTotal.WithLabelValues(sub).Inc()
	}

	return Result{SessionID: sessionID, Step: step, Outcome: outcome, Band: band, Model: req.Model}
}

// Metrics exposes the Tracker's Metrics instance so main.go can record
// Stage 4 activity (request outcomes) that has nothing to do with session
// state, without this package growing an HTTP-shaped API of its own.
func (t *Tracker) Metrics() *Metrics {
	return t.metrics
}

// assignBand is TASK-qos-fair-share.md §4.2 step 2 plus §4.4's spend
// modulation. Demotion (by streak or by spend) is checked before warmth,
// which inverts the doc's literal listing order (warm, then demoted, else
// normal): a session dispatched on every consecutive request -- the
// ordinary shape of a coding agent mid-task -- would always read as
// "recently dispatched" under a warm-first check, so demotion could never
// actually trigger and requirement 3 (Stage 5 test 7: a second session must
// get a slot by the first session's (N+1)th consecutive step) would fail by
// construction. Checking demotion first makes it win once triggered,
// regardless of warmth.
//
// The doc's cached_tokens correction for warm (§4.2 step 2, §5.2) is not
// implemented: ADR 0008 "Verifying the prediction" found this vLLM build
// never populates usage.prompt_tokens_details.cached_tokens, so the chain
// match plus WARM_TTL is the only warmth signal that exists on this engine.
func (t *Tracker) assignBand(ctx context.Context, sub, sessionID string) Band {
	streak, err := t.store.BumpStreak(ctx, sub, sessionID, t.ttl)
	if err != nil {
		// Fail open, per this package's existing rule: a Valkey error
		// degrades band assignment to "not demoted this request," never
		// the proxied request itself.
		streak = 0
	}
	sinceLast, hadPrev, dispatchErr := t.store.TouchDispatch(ctx, sub, sessionID, time.Now(), t.ttl)

	demotedByStreak := streak > t.demoteAfterSteps
	demotedBySpend := t.aheadOnSpend(ctx, sub)

	if demotedByStreak || demotedBySpend {
		if demotedByStreak && streak == t.demoteAfterSteps+1 {
			// Fires exactly once per streak-triggered demotion, not on
			// every subsequent request while the session stays demoted.
			// Spend-triggered demotion is not separately counted here --
			// it has no single crossing point to fire once on (the gap to
			// the group minimum moves every request, for every user), and
			// Stage 4's dashboard is the place for a spend-specific view,
			// not this counter.
			t.metrics.RotationsTotal.WithLabelValues(sub).Inc()
		}
		return BandDemoted
	}
	if dispatchErr == nil && hadPrev && sinceLast <= t.warmTTL {
		return BandWarm
	}
	return BandNormal
}

// aheadOnSpend is TASK-qos-fair-share.md §4.4: "пользователь, ушедший
// далеко вперёд по расходу, временно получает полосу ниже." It compares
// sub's own recent spend against the lowest recent spend among other
// currently active users -- not a fixed budget, since this is modulation
// of the priority queue, not a limit (limits are Envoy AI Gateway's job,
// §2). A lone active user is never demoted by this check: there is no one
// to be ahead of, and demoting the only active user would make the queue
// less work-conserving for no fairness benefit.
func (t *Tracker) aheadOnSpend(ctx context.Context, sub string) bool {
	mySpend, err := t.store.GetSpend(ctx, sub)
	if err != nil {
		return false
	}
	minSpend, hasOthers, err := t.store.MinActiveSpend(ctx, sub)
	if err != nil || !hasOthers {
		return false
	}
	return mySpend-minSpend > t.spendDemoteThreshold
}

// RecordCost is TASK-qos-fair-share.md §4.4: it converts a completed
// request's measured usage into the same cost unit assignBand's spend
// modulation reads, so the priority queue and any future dashboard agree
// on what "expensive" means. Cost can only be known once the response
// completes, so this updates sub's spend for *future* requests to compare
// against -- it never affects the request that produced it. cachedTokens
// is accepted for completeness even though this vLLM build never reports
// it (ADR 0008 "Verifying the prediction"); the formula subtracts it as
// specified regardless. It also emits Stage 4's per-user token and cost
// counters, labeled by model for the activity dashboard.
func (t *Tracker) RecordCost(ctx context.Context, sub, model string, promptTokens, cachedTokens, completionTokens int64) {
	t.metrics.PromptTokensTotal.WithLabelValues(sub, model).Add(float64(promptTokens))
	t.metrics.CachedPromptTokensTotal.WithLabelValues(sub, model).Add(float64(cachedTokens))
	t.metrics.CompletionTokensTotal.WithLabelValues(sub, model).Add(float64(completionTokens))

	uncached := promptTokens - cachedTokens
	if uncached < 0 {
		uncached = 0
	}
	cost := t.costAlpha*float64(uncached) + t.costBeta*float64(completionTokens)
	if cost <= 0 {
		return
	}
	t.metrics.CostUnitsTotal.WithLabelValues(sub, model).Add(cost)
	_, _ = t.store.AddSpend(ctx, sub, cost, t.spendWindow)
}

func newSessionID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand.Read failing means the OS entropy source is broken;
		// still return a usable, merely non-random id rather than crash a
		// request path over an observation-only feature.
		return "sess-fallback"
	}
	return "sess-" + hex.EncodeToString(b)
}
