package qos

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

// fakeStore is an in-memory Store for unit tests -- no Valkey required.
type fakeStore struct {
	chains       map[string]string // chainKey(sub,link) -> session id
	steps        map[string]int64  // stepsKey(sub,session) -> count
	getErr       error
	streakSess   map[string]string // sub -> last session id
	streakCount  map[string]int64  // sub -> streak count
	dispatchedAt map[string]time.Time
	spend        map[string]float64
	active       map[string]bool
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		chains:       map[string]string{},
		steps:        map[string]int64{},
		streakSess:   map[string]string{},
		streakCount:  map[string]int64{},
		dispatchedAt: map[string]time.Time{},
		spend:        map[string]float64{},
		active:       map[string]bool{},
	}
}

func (f *fakeStore) GetSessionForChain(_ context.Context, sub, link string) (string, error) {
	if f.getErr != nil {
		return "", f.getErr
	}
	return f.chains[chainKey(sub, link)], nil
}

func (f *fakeStore) SetSessionForChain(_ context.Context, sub, link, sessionID string, _ time.Duration) error {
	f.chains[chainKey(sub, link)] = sessionID
	return nil
}

func (f *fakeStore) IncrStep(_ context.Context, sub, sessionID string, _ time.Duration) (int64, error) {
	key := stepsKey(sub, sessionID)
	f.steps[key]++
	return f.steps[key], nil
}

func (f *fakeStore) TouchActive(_ context.Context, sub string, _ string, _ time.Time) error {
	f.active[sub] = true
	return nil
}

func (f *fakeStore) GetSpend(_ context.Context, sub string) (float64, error) {
	return f.spend[sub], nil
}

func (f *fakeStore) AddSpend(_ context.Context, sub string, cost float64, _ time.Duration) (float64, error) {
	f.spend[sub] += cost
	return f.spend[sub], nil
}

func (f *fakeStore) MinActiveSpend(_ context.Context, excludeSub string) (float64, bool, error) {
	min := 0.0
	found := false
	for sub := range f.active {
		if sub == excludeSub {
			continue
		}
		if v := f.spend[sub]; !found || v < min {
			min = v
			found = true
		}
	}
	return min, found, nil
}

func (f *fakeStore) BumpStreak(_ context.Context, sub, sessionID string, _ time.Duration) (int64, error) {
	if f.streakSess[sub] == sessionID {
		f.streakCount[sub]++
	} else {
		f.streakSess[sub] = sessionID
		f.streakCount[sub] = 1
	}
	return f.streakCount[sub], nil
}

func (f *fakeStore) TouchDispatch(_ context.Context, sub, sessionID string, now time.Time, _ time.Duration) (time.Duration, bool, error) {
	key := sub + ":" + sessionID
	prev, ok := f.dispatchedAt[key]
	f.dispatchedAt[key] = now
	if !ok {
		return 0, false, nil
	}
	return now.Sub(prev), true, nil
}

func newTestMetrics() *Metrics {
	return NewMetrics(prometheus.NewRegistry())
}

func TestObserveMintsNewSessionOnFirstRequest(t *testing.T) {
	tr := NewTracker(newFakeStore(), newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	res := tr.Observe(context.Background(), "alice", body)

	if res.Outcome != New {
		t.Fatalf("Outcome = %s, want New", res.Outcome)
	}
	if res.SessionID == "" {
		t.Fatal("New outcome did not mint a session id")
	}
	if res.Step != 1 {
		t.Fatalf("Step = %d, want 1", res.Step)
	}
}

func TestObserveMatchesSecondStepOfSameSession(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)

	first := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))

	second := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"more"}]}`))

	if second.Outcome != Matched {
		t.Fatalf("Outcome = %s, want Matched", second.Outcome)
	}
	if second.SessionID != first.SessionID {
		t.Fatalf("session id changed across steps of the same conversation: %s -> %s", first.SessionID, second.SessionID)
	}
	if second.Step != 2 {
		t.Fatalf("Step = %d, want 2", second.Step)
	}
}

func TestObserveSurvivesHistoryCompactionViaC0(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)

	tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"go deep"},{"role":"assistant","content":"..."}]}`))

	// Client compacted history: kept only the first message, dropped the
	// middle, appended a new one. c_0 still matches.
	compacted := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"user","content":"summarized so far, continue"}]}`))

	if compacted.Outcome != Matched {
		t.Fatalf("Outcome after compaction = %s, want Matched (c_0 should still match)", compacted.Outcome)
	}
}

func TestObserveKeepsSessionsOfDifferentUsersSeparate(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	alice := tr.Observe(context.Background(), "alice", body)
	bob := tr.Observe(context.Background(), "bob", body)

	if alice.Outcome != New || bob.Outcome != New {
		t.Fatalf("both users' first identical request should be New, got alice=%s bob=%s", alice.Outcome, bob.Outcome)
	}
	if alice.SessionID == bob.SessionID {
		t.Fatal("identical message content from two different users collapsed into one session")
	}
}

func TestObserveDegradesOnNonChatBody(t *testing.T) {
	tr := NewTracker(newFakeStore(), newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	res := tr.Observe(context.Background(), "alice", []byte(`{"model":"m","prompt":"hi"}`))
	if res.Outcome != Degraded {
		t.Fatalf("Outcome = %s, want Degraded for a non-chat body", res.Outcome)
	}
}

func TestObserveDegradesOnStoreErrorWithoutPanicking(t *testing.T) {
	store := newFakeStore()
	store.getErr = errors.New("valkey unavailable")
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)

	res := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))

	if res.Outcome != Degraded {
		t.Fatalf("Outcome = %s, want Degraded when the store errors", res.Outcome)
	}
}

func TestAssignBandNormalOnFirstRequest(t *testing.T) {
	tr := NewTracker(newFakeStore(), newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	res := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if res.Band != BandNormal {
		t.Fatalf("Band = %s, want normal on a session's first request (nothing to be warm from)", res.Band)
	}
}

func TestAssignBandWarmsRecentSecondStep(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	tr.Observe(context.Background(), "alice", body)
	second := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"more"}]}`))

	if second.Band != BandWarm {
		t.Fatalf("Band = %s, want warm for a session dispatched well within WARM_TTL", second.Band)
	}
}

func TestAssignBandNotWarmAfterTTLExpires(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 1*time.Nanosecond, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	tr.Observe(context.Background(), "alice", body)
	time.Sleep(time.Millisecond)
	second := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"hello"},{"role":"user","content":"more"}]}`))

	if second.Band != BandNormal {
		t.Fatalf("Band = %s, want normal once WARM_TTL has elapsed since the last dispatch", second.Band)
	}
}

func TestAssignBandDemotesAfterConsecutiveSteps(t *testing.T) {
	store := newFakeStore()
	metrics := newTestMetrics()
	const demoteAfter = 2
	tr := NewTracker(store, metrics, 30*time.Minute, 120*time.Second, demoteAfter, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	var last Result
	for i := 0; i < demoteAfter+1; i++ {
		last = tr.Observe(context.Background(), "alice", body)
	}

	if last.Band != BandDemoted {
		t.Fatalf("Band = %s, want demoted on the (demoteAfterSteps+1)th consecutive step of the same session", last.Band)
	}
	if got := testutil.ToFloat64(metrics.RotationsTotal.WithLabelValues("alice")); got != 1 {
		t.Fatalf("patsvc_rotations_total{user=alice} = %v, want 1 (demotion fires once per crossing)", got)
	}
}

func TestAssignBandDoesNotReDemoteEveryRequest(t *testing.T) {
	store := newFakeStore()
	metrics := newTestMetrics()
	const demoteAfter = 2
	tr := NewTracker(store, metrics, 30*time.Minute, 120*time.Second, demoteAfter, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	for i := 0; i < demoteAfter+3; i++ {
		tr.Observe(context.Background(), "alice", body)
	}

	if got := testutil.ToFloat64(metrics.RotationsTotal.WithLabelValues("alice")); got != 1 {
		t.Fatalf("patsvc_rotations_total{user=alice} = %v, want 1 even after staying demoted for several more steps", got)
	}
}

func TestAssignBandStreakResetsWhenSessionChanges(t *testing.T) {
	store := newFakeStore()
	metrics := newTestMetrics()
	const demoteAfter = 2
	tr := NewTracker(store, metrics, 30*time.Minute, 120*time.Second, demoteAfter, 9.4e-5, 0.0149, 5.0, 10*time.Minute)

	sessionA := []byte(`{"model":"m","messages":[{"role":"user","content":"session-a"}]}`)
	sessionB := []byte(`{"model":"m","messages":[{"role":"user","content":"session-b"}]}`)

	tr.Observe(context.Background(), "alice", sessionA)
	tr.Observe(context.Background(), "alice", sessionA)
	// A different session for the same user resets the streak: A was one
	// step short of demotion, but switching away must not carry that
	// count over to B.
	res := tr.Observe(context.Background(), "alice", sessionB)

	if res.Band == BandDemoted {
		t.Fatal("a brand-new session inherited the previous session's streak")
	}
}

func TestAssignBandDemotesFarAheadOnSpend(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	// alice has spent far more than bob and both are active.
	tr.Observe(context.Background(), "bob", body)
	store.spend["alice"] = 100

	res := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"different"}]}`))

	if res.Band != BandDemoted {
		t.Fatalf("Band = %s, want demoted for a user far ahead of another active user on spend", res.Band)
	}
}

func TestAssignBandDoesNotDemoteSoleActiveUserOnSpend(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)

	store.spend["alice"] = 1000 // huge spend, but no one else is active to be ahead of
	res := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))

	if res.Band == BandDemoted {
		t.Fatal("a lone active user was demoted on spend with no one to be ahead of")
	}
}

func TestAssignBandDoesNotDemoteWithinSpendThreshold(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 9.4e-5, 0.0149, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	tr.Observe(context.Background(), "bob", body)
	store.spend["alice"] = 2 // ahead of bob's 0, but under the threshold of 5

	res := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"different"}]}`))

	if res.Band == BandDemoted {
		t.Fatal("a user only slightly ahead on spend, within the threshold, was demoted")
	}
}

func TestRecordCostFeedsFutureSpendModulation(t *testing.T) {
	store := newFakeStore()
	tr := NewTracker(store, newTestMetrics(), 30*time.Minute, 120*time.Second, 8, 1.0, 1.0, 5.0, 10*time.Minute)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	tr.Observe(context.Background(), "bob", body)
	// alpha=beta=1.0: cost = (60-10) + 5 = 55, well over the threshold of 5.
	tr.RecordCost(context.Background(), "alice", "m", 60, 10, 5)

	res := tr.Observe(context.Background(), "alice", []byte(
		`{"model":"m","messages":[{"role":"user","content":"different"}]}`))

	if res.Band != BandDemoted {
		t.Fatalf("Band = %s, want demoted after RecordCost pushed alice far ahead of bob", res.Band)
	}
}
