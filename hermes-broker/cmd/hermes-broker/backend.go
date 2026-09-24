package main

import (
	"context"
	"time"
)

// Backend is the execution side of the broker (docs/adr/0014 section 10).
// Everything above it -- identity, slots, idle rules, /skills, catalog
// notices -- is backend-independent. `pods` (one pod per active user, one
// RWO PVC per profile) is the only implementation. A `substrate` backend maps
// Start/Stop onto CreateActor|ResumeActor/SuspendActor, and a kagent backend
// onto one AgentHarness (backend: hermes, runtime: substrate) per user; both
// keep this interface and the agent contract below.
//
// Agent contract, whatever runs it:
//   - Hermes `gateway run` with the OpenAI-compatible API server on
//     AgentPort, bearer-authenticated with the per-user key the backend
//     holds, `GET /health` for readiness;
//   - HERMES_HOME is the user's profile; hermes-sync from the pinned catalog
//     image has run against it before Hermes starts;
//   - at most one running copy per profile.
type Backend interface {
	// EnsureProfile creates the user's durable profile if it does not exist
	// and reports whether it was created now.
	EnsureProfile(ctx context.Context, u User) (created bool, err error)
	// Start runs the user's agent and blocks until it answers readiness.
	// If an agent is already running or starting it waits for that one.
	Start(ctx context.Context, u User, spec StartSpec) (*Agent, error)
	// Stop releases the agent's compute and blocks until it is gone. The
	// profile is kept.
	Stop(ctx context.Context, id string) error
	// List returns agents that currently hold compute, ready or not.
	List(ctx context.Context) ([]Agent, error)
	// Touch records last activity where a restarted broker can find it.
	Touch(ctx context.Context, id string, at time.Time) error
	// Profile returns the last sync report stored with the profile, or nil
	// if the agent never started.
	Profile(ctx context.Context, id string) (*SyncReport, error)
}

const AgentPort = 8642

type StartSpec struct {
	CatalogImage string
	// Selection, when non-nil, replaces the user's catalog-selection.yaml at
	// this start.
	Selection *Selection
}

type Agent struct {
	UserID       string
	Endpoint     string // base URL, e.g. http://10.0.0.5:8642
	APIKey       string
	Ready        bool
	CatalogImage string
	StartedAt    time.Time
	LastActivity time.Time
	Report       *SyncReport
}

type Selection struct {
	Disabled []string `json:"disabled"`
	Enabled  []string `json:"enabled"`
}

// SyncReport is what hermes-sync prints at the end of every start.
type SyncReport struct {
	Version  string    `json:"v"`
	On       []string  `json:"on"`
	Sel      Selection `json:"sel"`
	New      []string  `json:"new"`
	Shadowed []string  `json:"shadowed"`
}
