package main

import (
	"context"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"
)

var errNoSlot = errors.New("no free agent slot")

type SlotsConfig struct {
	K                int
	IdleTimeout      time.Duration // idle this long -> evictable under pressure
	IdleShutdown     time.Duration // idle this long -> stopped regardless
	MaxAgentLifetime time.Duration // stale-by-age agents restart at next idle
	SlotWaitTimeout  time.Duration
	CatalogImage     func() string
}

type entry struct {
	agent    *Agent // nil while not running
	inflight int
	last     time.Time
	touched  time.Time // last Touch persisted to the backend
	busy     bool      // starting or stopping; holds the slot
	// pending selection to apply at the next start (forces a restart).
	pending *Selection
	// notice to prefix to the next reply (new catalog skills).
	notice []string
}

// Slots enforces at most K running agents (docs/adr/0014 sections 5-7): an
// agent is started on demand, the least recently used idle one is evicted
// when a slot is needed, and requests queue FIFO for SlotWaitTimeout when
// every agent is busy. State is in memory; after a broker restart every
// running agent counts as active for one IdleTimeout.
type Slots struct {
	cfg SlotsConfig
	b   Backend
	now func() time.Time

	mu      sync.Mutex
	users   map[string]*entry
	queue   []chan struct{}
	changed chan struct{}
}

func NewSlots(cfg SlotsConfig, b Backend) *Slots {
	return &Slots{cfg: cfg, b: b, now: time.Now, users: map[string]*entry{}, changed: make(chan struct{})}
}

// Recover rebuilds the running set from the backend after a broker restart.
func (s *Slots) Recover(ctx context.Context) error {
	agents, err := s.b.List(ctx)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range agents {
		a := agents[i]
		// Busy for one idle timeout: in-flight counts were lost with the
		// previous process, so nothing is evicted blindly.
		s.users[a.UserID] = &entry{agent: &a, last: s.now(), touched: s.now()}
	}
	slog.Info("recovered agents", "count", len(agents))
	return nil
}

func (s *Slots) get(id string) *entry {
	e := s.users[id]
	if e == nil {
		e = &entry{}
		s.users[id] = e
	}
	return e
}

func (s *Slots) usedLocked() int {
	n := 0
	for _, e := range s.users {
		if e.agent != nil || e.busy {
			n++
		}
	}
	return n
}

func (s *Slots) idle(e *entry, now time.Time, after time.Duration) bool {
	return e.agent != nil && !e.busy && e.inflight == 0 && now.Sub(e.last) >= after
}

// lruIdleLocked picks the least recently used agent that is idle by
// IdleTimeout. Returns "" if none.
func (s *Slots) lruIdleLocked(now time.Time) string {
	best, bestAt := "", time.Time{}
	for id, e := range s.users {
		if s.idle(e, now, s.cfg.IdleTimeout) && (best == "" || e.last.Before(bestAt)) {
			best, bestAt = id, e.last
		}
	}
	return best
}

func (s *Slots) notifyLocked() {
	close(s.changed)
	s.changed = make(chan struct{})
}

// Acquire returns a ready agent for u with one in-flight request counted
// against it; the caller must Release it. onWait is called once if the
// request has to queue.
func (s *Slots) Acquire(ctx context.Context, u User, onWait func()) (*Agent, []string, error) {
	deadline := time.Now().Add(s.cfg.SlotWaitTimeout) // wall clock, not s.now
	var ticket chan struct{}
	defer func() {
		if ticket != nil {
			s.mu.Lock()
			s.dequeueLocked(ticket)
			s.mu.Unlock()
		}
	}()
	for {
		s.mu.Lock()
		e := s.get(u.ID)
		now := s.now()
		if e.agent != nil && !e.busy && e.pending == nil && e.agent.APIKey != "" {
			e.inflight++
			e.last = now
			notice := e.notice
			e.notice = nil
			a := e.agent
			s.mu.Unlock()
			s.touch(ctx, u.ID, e, now)
			return a, notice, nil
		}
		headOfQueue := len(s.queue) == 0 || (ticket != nil && s.queue[0] == ticket)
		switch {
		case e.busy:
			// Another request of this user is starting/stopping the agent.
		case e.agent != nil && e.agent.APIKey == "" && e.pending == nil:
			// Recovered after a broker restart: attach to the running agent.
			e.busy = true
			s.mu.Unlock()
			if err := s.start(ctx, u, e, nil); err != nil {
				return nil, nil, err
			}
			continue
		case e.agent != nil && e.pending != nil && e.inflight == 0:
			// Selection change: restart once the user's streams are done.
			e.busy = true
			s.mu.Unlock()
			if err := s.stop(ctx, u.ID, e, "selection change"); err != nil {
				return nil, nil, err
			}
			continue
		case e.agent != nil && e.pending != nil:
			// Wait for this user's other streams to finish.
		case headOfQueue && s.usedLocked() < s.cfg.K:
			e.busy = true
			sel := e.pending
			s.mu.Unlock()
			err := s.start(ctx, u, e, sel)
			if err != nil {
				return nil, nil, err
			}
			continue
		case headOfQueue:
			if victim := s.lruIdleLocked(now); victim != "" {
				ve := s.users[victim]
				ve.busy = true
				s.mu.Unlock()
				if err := s.stop(ctx, victim, ve, "evicted for another user"); err != nil {
					return nil, nil, err
				}
				continue
			}
		}
		if ticket == nil && e.agent == nil && !e.busy {
			ticket = make(chan struct{})
			s.queue = append(s.queue, ticket)
			if onWait != nil {
				onWait()
			}
			// A slot may be held by an agent that is already gone.
			go s.reconcile(context.WithoutCancel(ctx))
		}
		changed := s.changed
		s.mu.Unlock()

		wait := time.Until(deadline)
		if wait <= 0 {
			return nil, nil, errNoSlot
		}
		timer := time.NewTimer(min(wait, 5*time.Second))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, nil, ctx.Err()
		case <-changed:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func (s *Slots) dequeueLocked(t chan struct{}) {
	for i, q := range s.queue {
		if q == t {
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			s.notifyLocked()
			return
		}
	}
}

func (s *Slots) start(ctx context.Context, u User, e *entry, sel *Selection) error {
	began := time.Now()
	a, err := s.b.Start(ctx, u, StartSpec{CatalogImage: s.cfg.CatalogImage(), Selection: sel})
	if err != nil {
		// Do not leave a half-started agent holding capacity outside the count.
		if serr := s.b.Stop(context.WithoutCancel(ctx), u.ID); serr != nil {
			slog.Error("cleanup after failed start", "user", u.ID, "err", serr)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	e.busy = false
	defer s.notifyLocked()
	if err != nil {
		slog.Error("agent start failed", "user", u.ID, "err", err)
		e.agent = nil
		return err
	}
	e.agent = a
	e.last = s.now()
	if sel != nil && e.pending == sel {
		e.pending = nil
	}
	if a.Report != nil && len(a.Report.New) > 0 {
		e.notice = a.Report.New
	}
	slog.Info("agent started", "user", u.ID, "catalog", a.CatalogImage, "seconds", time.Since(began).Seconds())
	return nil
}

func (s *Slots) stop(ctx context.Context, id string, e *entry, reason string) error {
	err := s.b.Stop(ctx, id)
	s.mu.Lock()
	defer s.mu.Unlock()
	e.busy = false
	if err != nil {
		slog.Error("agent stop failed", "user", id, "err", err)
	} else {
		e.agent = nil
		slog.Info("agent stopped", "user", id, "reason", reason)
	}
	s.notifyLocked()
	return err
}

func (s *Slots) Release(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.users[id]; e != nil {
		e.inflight--
		e.last = s.now()
	}
	s.notifyLocked()
}

func (s *Slots) touch(ctx context.Context, id string, e *entry, now time.Time) {
	s.mu.Lock()
	due := now.Sub(e.touched) >= time.Minute
	if due {
		e.touched = now
	}
	s.mu.Unlock()
	if due {
		if err := s.b.Touch(ctx, id, now); err != nil {
			slog.Warn("touch failed", "user", id, "err", err)
		}
	}
}

// SetPending records a selection to apply at the user's next start; a
// running agent is restarted before its next request is served.
func (s *Slots) SetPending(id string, sel Selection) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.get(id).pending = &sel
}

// Pending returns the not-yet-applied selection, if any.
func (s *Slots) Pending(id string) *Selection {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.users[id]; e != nil {
		return e.pending
	}
	return nil
}

// Running returns the running agent's report, if the user has one.
func (s *Slots) Running(id string) *Agent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.users[id]; e != nil && e.agent != nil {
		return e.agent
	}
	return nil
}

// reconcile forgets agents whose compute disappeared outside the broker
// (offboarding script, eviction, node loss), so they stop holding slots.
func (s *Slots) reconcile(ctx context.Context) {
	agents, err := s.b.List(ctx)
	if err != nil {
		slog.Warn("reconcile: list agents", "err", err)
		return
	}
	alive := map[string]bool{}
	for _, a := range agents {
		alive[a.UserID] = true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for id, e := range s.users {
		if e.agent != nil && !e.busy && !alive[id] {
			slog.Info("agent gone outside the broker", "user", id)
			e.agent = nil
			e.inflight = 0
			changed = true
		}
	}
	if changed {
		s.notifyLocked()
	}
}

// Reap stops agents idle past IdleShutdown and agents that are stale (old
// catalog image or older than MaxAgentLifetime) at their first idle moment.
func (s *Slots) Reap(ctx context.Context) {
	s.reconcile(ctx)
	now := s.now()
	current := s.cfg.CatalogImage()
	type victim struct {
		id     string
		e      *entry
		reason string
	}
	var victims []victim
	s.mu.Lock()
	for id, e := range s.users {
		if e.agent == nil || e.busy || e.inflight > 0 {
			continue
		}
		switch {
		case now.Sub(e.last) >= s.cfg.IdleShutdown:
			victims = append(victims, victim{id, e, "idle shutdown"})
		case e.agent.CatalogImage != current:
			victims = append(victims, victim{id, e, "stale catalog"})
		case !e.agent.StartedAt.IsZero() && now.Sub(e.agent.StartedAt) >= s.cfg.MaxAgentLifetime:
			victims = append(victims, victim{id, e, "max lifetime"})
		default:
			continue
		}
		e.busy = true
	}
	s.mu.Unlock()
	sort.Slice(victims, func(i, j int) bool { return victims[i].id < victims[j].id })
	for _, v := range victims {
		_ = s.stop(ctx, v.id, v.e, v.reason)
	}
}

type SlotStats struct {
	K       int `json:"k"`
	Running int `json:"running"`
	Busy    int `json:"busy"`
	Queued  int `json:"queued"`
}

func (s *Slots) Stats() SlotStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := SlotStats{K: s.cfg.K, Queued: len(s.queue)}
	for _, e := range s.users {
		if e.agent != nil {
			st.Running++
		}
		if e.inflight > 0 {
			st.Busy++
		}
	}
	return st
}
