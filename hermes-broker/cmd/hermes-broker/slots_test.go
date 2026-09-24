package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeBackend struct {
	mu      sync.Mutex
	running map[string]*Agent
	starts  map[string]int
	stops   []string
	lastSel map[string]*Selection
}

func newFake() *fakeBackend {
	return &fakeBackend{running: map[string]*Agent{}, starts: map[string]int{}, lastSel: map[string]*Selection{}}
}

func (f *fakeBackend) EnsureProfile(context.Context, User) (bool, error) { return false, nil }
func (f *fakeBackend) Start(_ context.Context, u User, spec StartSpec) (*Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.running) >= 2 && f.running[u.ID] == nil {
		return nil, errors.New("backend over capacity")
	}
	a := &Agent{UserID: u.ID, Endpoint: "http://" + u.ID, APIKey: "k", Ready: true,
		CatalogImage: spec.CatalogImage, StartedAt: time.Now()}
	f.running[u.ID] = a
	f.starts[u.ID]++
	f.lastSel[u.ID] = spec.Selection
	return a, nil
}
func (f *fakeBackend) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, id)
	f.stops = append(f.stops, id)
	return nil
}
func (f *fakeBackend) List(context.Context) ([]Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Agent
	for _, a := range f.running {
		c := *a
		c.APIKey = ""
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeBackend) Touch(context.Context, string, time.Time) error       { return nil }
func (f *fakeBackend) Profile(context.Context, string) (*SyncReport, error) { return nil, nil }

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newTestSlots(b Backend, c *clock) *Slots {
	s := NewSlots(SlotsConfig{K: 2, IdleTimeout: 15 * time.Minute, IdleShutdown: 30 * time.Minute,
		MaxAgentLifetime: 8 * time.Hour, SlotWaitTimeout: 200 * time.Millisecond,
		CatalogImage: func() string { return "cat:1" }}, b)
	s.now = c.now
	return s
}

func user(id string) User { return User{Sub: id, ID: id} }

func TestSlotsLRUEvictionAndQueue(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(), &clock{time.Unix(1_000_000, 0)}
	s := newTestSlots(f, c)

	for _, id := range []string{"a", "b"} {
		if _, _, err := s.Acquire(ctx, user(id), nil); err != nil {
			t.Fatal(err)
		}
		s.Release(id)
		c.t = c.t.Add(time.Minute)
	}
	// Both agents busy -> the third user queues and gets errNoSlot.
	_, _, _ = s.Acquire(ctx, user("a"), nil)
	_, _, _ = s.Acquire(ctx, user("b"), nil)
	waited := false
	if _, _, err := s.Acquire(ctx, user("c"), func() { waited = true }); !errors.Is(err, errNoSlot) || !waited {
		t.Fatalf("want queued errNoSlot, got %v waited=%v", err, waited)
	}
	s.Release("a")
	s.Release("b")

	// Idle but not past IdleTimeout -> still no eviction.
	if _, _, err := s.Acquire(ctx, user("c"), nil); !errors.Is(err, errNoSlot) {
		t.Fatalf("evicted a recently active agent: %v", err)
	}
	// Past IdleTimeout: the least recently used (a) is evicted.
	c.t = c.t.Add(20 * time.Minute)
	if _, _, err := s.Acquire(ctx, user("c"), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.stops) != 1 || f.stops[0] != "a" {
		t.Fatalf("evicted %v, want [a]", f.stops)
	}
	if st := s.Stats(); st.Running != 2 {
		t.Fatalf("running %d, want 2", st.Running)
	}
}

func TestSlotsSelectionRestartWaitsForStreams(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(), &clock{time.Unix(1_000_000, 0)}
	s := newTestSlots(f, c)
	if _, _, err := s.Acquire(ctx, user("a"), nil); err != nil { // open stream
		t.Fatal(err)
	}
	s.SetPending("a", Selection{Disabled: []string{"repo-reader"}})
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Acquire(ctx, user("a"), nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if len(f.stops) != 0 {
		t.Fatal("restarted while a stream was open")
	}
	s.Release("a")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.starts["a"] != 2 || f.lastSel["a"] == nil || f.lastSel["a"].Disabled[0] != "repo-reader" {
		t.Fatalf("starts=%d sel=%+v", f.starts["a"], f.lastSel["a"])
	}
	if s.Pending("a") != nil {
		t.Fatal("pending selection not cleared after start")
	}
}

func TestSlotsReapAndRecover(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(), &clock{time.Unix(1_000_000, 0)}
	s := newTestSlots(f, c)
	_, _, _ = s.Acquire(ctx, user("a"), nil)
	s.Release("a")

	// A restarted broker treats recovered agents as active for IdleTimeout.
	s2 := newTestSlots(f, c)
	if err := s2.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	s2.Reap(ctx)
	if len(f.stops) != 0 {
		t.Fatal("recovered agent reaped immediately")
	}
	// Recovered agent has no key yet; Acquire re-attaches without a new pod.
	a, _, err := s2.Acquire(ctx, user("a"), nil)
	if err != nil || a.APIKey == "" {
		t.Fatalf("attach: %+v %v", a, err)
	}
	s2.Release("a")

	// Catalog changed -> stale agent stopped at idle.
	s2.cfg.CatalogImage = func() string { return "cat:2" }
	s2.Reap(ctx)
	if len(f.stops) != 1 {
		t.Fatalf("stale agent not stopped: %v", f.stops)
	}
}

func TestSlotsForgetAgentsDeletedOutsideBroker(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(), &clock{time.Unix(1_000_000, 0)}
	s := newTestSlots(f, c)
	for _, id := range []string{"a", "b"} {
		if _, _, err := s.Acquire(ctx, user(id), nil); err != nil {
			t.Fatal(err)
		}
		s.Release(id)
	}
	// Offboarding deletes a's pod behind the broker's back.
	_ = f.Stop(ctx, "a")
	if _, _, err := s.Acquire(ctx, user("c"), nil); err != nil {
		t.Fatalf("slot of a deleted agent was not reclaimed: %v", err)
	}
}
