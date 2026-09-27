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

// The fake keys agents by ref.String(): "hermes/a".
func (f *fakeBackend) EnsureProfile(context.Context, User, AgentRef) (bool, error) { return false, nil }
func (f *fakeBackend) Start(_ context.Context, _ User, ref AgentRef, spec StartSpec) (*Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := ref.String()
	if len(f.running) >= 2 && f.running[id] == nil {
		return nil, errors.New("backend over capacity")
	}
	a := &Agent{Ref: ref, Endpoint: "http://" + id, APIKey: "k", Ready: true,
		CatalogImage: spec.CatalogImage, StartedAt: time.Now()}
	f.running[id] = a
	f.starts[id]++
	f.lastSel[id] = spec.Selection
	return a, nil
}
func (f *fakeBackend) Stop(_ context.Context, ref AgentRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.running, ref.String())
	f.stops = append(f.stops, ref.String())
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
func (f *fakeBackend) Touch(context.Context, AgentRef, time.Time) error       { return nil }
func (f *fakeBackend) Profile(context.Context, AgentRef) (*SyncReport, error) { return nil, nil }

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

func hermes(id string) AgentRef { return AgentRef{Runtime: "hermes", ID: id} }

func TestSlotsLRUEvictionAndQueue(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(), &clock{time.Unix(1_000_000, 0)}
	s := newTestSlots(f, c)

	for _, id := range []string{"a", "b"} {
		if _, _, err := s.Acquire(ctx, user(id), hermes(id), nil); err != nil {
			t.Fatal(err)
		}
		s.Release(hermes(id))
		c.t = c.t.Add(time.Minute)
	}
	// Both agents busy -> the third user queues and gets errNoSlot.
	_, _, _ = s.Acquire(ctx, user("a"), hermes("a"), nil)
	_, _, _ = s.Acquire(ctx, user("b"), hermes("b"), nil)
	waited := false
	if _, _, err := s.Acquire(ctx, user("c"), hermes("c"), func() { waited = true }); !errors.Is(err, errNoSlot) || !waited {
		t.Fatalf("want queued errNoSlot, got %v waited=%v", err, waited)
	}
	s.Release(hermes("a"))
	s.Release(hermes("b"))

	// Idle but not past IdleTimeout -> still no eviction.
	if _, _, err := s.Acquire(ctx, user("c"), hermes("c"), nil); !errors.Is(err, errNoSlot) {
		t.Fatalf("evicted a recently active agent: %v", err)
	}
	// Past IdleTimeout: the least recently used (a) is evicted.
	c.t = c.t.Add(20 * time.Minute)
	if _, _, err := s.Acquire(ctx, user("c"), hermes("c"), nil); err != nil {
		t.Fatal(err)
	}
	if len(f.stops) != 1 || f.stops[0] != "hermes/a" {
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
	if _, _, err := s.Acquire(ctx, user("a"), hermes("a"), nil); err != nil { // open stream
		t.Fatal(err)
	}
	s.SetPending(hermes("a"), Selection{Disabled: []string{"repo-reader"}})
	done := make(chan error, 1)
	go func() {
		_, _, err := s.Acquire(ctx, user("a"), hermes("a"), nil)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if len(f.stops) != 0 {
		t.Fatal("restarted while a stream was open")
	}
	s.Release(hermes("a"))
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.starts["hermes/a"] != 2 || f.lastSel["hermes/a"] == nil || f.lastSel["hermes/a"].Disabled[0] != "repo-reader" {
		t.Fatalf("starts=%d sel=%+v", f.starts["hermes/a"], f.lastSel["hermes/a"])
	}
	if s.Pending(hermes("a")) != nil {
		t.Fatal("pending selection not cleared after start")
	}
}

func TestSlotsReapAndRecover(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(), &clock{time.Unix(1_000_000, 0)}
	s := newTestSlots(f, c)
	_, _, _ = s.Acquire(ctx, user("a"), hermes("a"), nil)
	s.Release(hermes("a"))

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
	a, _, err := s2.Acquire(ctx, user("a"), hermes("a"), nil)
	if err != nil || a.APIKey == "" {
		t.Fatalf("attach: %+v %v", a, err)
	}
	s2.Release(hermes("a"))

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
		if _, _, err := s.Acquire(ctx, user(id), hermes(id), nil); err != nil {
			t.Fatal(err)
		}
		s.Release(hermes(id))
	}
	// Offboarding deletes a's pod behind the broker's back.
	_ = f.Stop(ctx, hermes("a"))
	if _, _, err := s.Acquire(ctx, user("c"), hermes("c"), nil); err != nil {
		t.Fatalf("slot of a deleted agent was not reclaimed: %v", err)
	}
}

func TestSlotsOneAgentPerRuntime(t *testing.T) {
	ctx := context.Background()
	f, c := newFake(), &clock{time.Unix(1_000_000, 0)}
	s := newTestSlots(f, c)
	pi := AgentRef{Runtime: "pi", ID: "a"}
	for _, ref := range []AgentRef{hermes("a"), pi} {
		a, _, err := s.Acquire(ctx, user("a"), ref, nil)
		if err != nil || a.Ref != ref {
			t.Fatalf("%v: %+v %v", ref, a, err)
		}
		s.Release(ref)
	}
	// Both of a's agents hold a slot of K=2.
	if st := s.Stats(); st.Running != 2 {
		t.Fatalf("running %d, want 2", st.Running)
	}
	if _, _, err := s.Acquire(ctx, user("b"), hermes("b"), nil); !errors.Is(err, errNoSlot) {
		t.Fatalf("want errNoSlot, got %v", err)
	}
}
