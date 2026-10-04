package main

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// seedLedger writes rows with known timing, all for subject "alice":
//
//	r1 streaming ok  [0s,6s]  first 1s last 5s, 9 output tokens, session s1
//	r2 streaming ok  [3s,8s]  first 5s last 7s, 3 output tokens, session s1
//	r3 non-streaming [20s,22s], 5 output tokens, no session
//	r4 recorded before timing existed (no timestamps), session s1
//	r5 cancelled     [30s,33s] first 31s, usage unknown, session s2
func seedLedger(t *testing.T, a *app, t0 time.Time) {
	t.Helper()
	at := func(s float64) *time.Time { v := t0.Add(time.Duration(s * float64(time.Second))); return &v }
	type row struct {
		session, outcome, quality       string
		streaming                       *bool
		completion                      int64
		received, first, last, finished *time.Time
		created                         time.Time
	}
	yes, no := true, false
	rows := []row{
		{"s1", "ok", "actual", &yes, 9, at(0), at(1), at(5), at(6), *at(6)},
		{"s1", "ok", "actual", &yes, 3, at(3), at(5), at(7), at(8), *at(8)},
		{"", "ok", "actual", &no, 5, at(20), nil, nil, at(22), *at(22)},
		{"s1", "ok", "actual", nil, 10, nil, nil, nil, nil, *at(40)},
		{"s2", "cancelled", "unknown", &yes, 0, at(30), at(31), nil, at(33), *at(33)},
	}
	for _, r := range rows {
		if _, err := a.db.Exec(context.Background(), `INSERT INTO qos_events
			(owner_issuer, owner_subject, owner_name, model, band, session_id, outcome, usage_quality, streaming, prompt_tokens, completion_tokens,
			 received_at, first_output_at, last_output_at, finished_at, created_at, source)
			VALUES ($1, 'alice', 'alice', 'qwen', 'normal', $2, $3, $4, $5, 100, $6, $7, $8, $9, $10, $11, 'pat')`,
			testIssuer, r.session, r.outcome, r.quality, r.streaming, r.completion, r.received, r.first, r.last, r.finished, r.created); err != nil {
			t.Fatal(err)
		}
	}
}

func near(got *float64, want float64) bool { return got != nil && math.Abs(*got-want) < 1e-6 }

func TestLedgerStatisticsFormulas(t *testing.T) {
	a, _ := migratedAdminApp(t)
	t0 := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	seedLedger(t, a, t0)
	ctx := context.Background()
	f := statsFilter{from: t0.Add(-time.Hour), to: t0.Add(time.Hour)}
	where, args := f.where(testIssuer, "alice")

	totals, err := a.queryTotals(ctx, where, args)
	if err != nil {
		t.Fatal(err)
	}
	if totals.Requests != 5 || totals.Outcomes["ok"] != 4 || totals.Outcomes["cancelled"] != 1 || totals.Sessions != 2 || totals.RequestsNoSession != 1 ||
		totals.UnknownUsage != 1 || totals.CompletionTokens != 27 {
		t.Errorf("totals = %+v", totals)
	}

	l, err := a.queryLatency(ctx, where, args)
	if err != nil {
		t.Fatal(err)
	}
	// TTFT samples 1, 2, 1 (r1, r2, r5); percentile_cont p95 of [1,1,2] = 1.9.
	if l.TTFTSamples != 3 || !near(l.TTFTp50, 1) || !near(l.TTFTp95, 1.9) {
		t.Errorf("ttft = %v/%v n=%d", deref(l.TTFTp50), deref(l.TTFTp95), l.TTFTSamples)
	}
	// TPOT: r1 (5-1)/(9-1)=0.5, r2 (7-5)/(3-1)=1.
	if l.TPOTSamples != 2 || !near(l.TPOTp50, 0.75) {
		t.Errorf("tpot = %v n=%d", deref(l.TPOTp50), l.TPOTSamples)
	}
	// Output rate is sum/sum: (9+3)/(5+4), not the mean of 1.8 and 0.75.
	if l.RateSamples != 2 || !near(l.OutputRate, 12.0/9.0) {
		t.Errorf("rate = %v n=%d", deref(l.OutputRate), l.RateSamples)
	}
	if l.NotMeasurable["recorded_before_timing"] != 1 || l.NotMeasurable["not_streaming"] != 1 || l.NotMeasurable["no_output"] != 0 {
		t.Errorf("not measurable = %v", l.NotMeasurable)
	}

	u, err := a.queryUsageTime(ctx, where, args)
	if err != nil {
		t.Fatal(err)
	}
	// Union [0,8] + [20,22] + [30,33] = 13; summed 6+5+2+3 = 16.
	if u.ActiveSeconds != 13 || u.SummedSeconds != 16 || u.TimedRequests != 4 {
		t.Errorf("usage time = %+v", u)
	}
	// s1 spans r1 start to r4 created (0s..40s), s2 30s..33s.
	if u.SessionSpanSeconds != 43 {
		t.Errorf("session span = %v, want 43", u.SessionSpanSeconds)
	}

	// Intervals are clipped to the range: from t0+4s keeps [4,8] of r1/r2.
	clipped := statsFilter{from: t0.Add(4 * time.Second), to: t0.Add(time.Hour)}
	w2, a2 := clipped.where(testIssuer, "alice")
	if u, err := a.queryUsageTime(ctx, w2, a2); err != nil || u.ActiveSeconds != 9 {
		t.Errorf("clipped active = %v, %v; want 4+2+3", u.ActiveSeconds, err)
	}

	timeline, err := a.queryTimeline(ctx, "hour", where, args)
	if err != nil || len(timeline) != 1 || timeline[0].Requests != 5 || timeline[0].Failed != 1 || !timeline[0].At.Equal(t0) {
		t.Errorf("timeline = %+v, %v", timeline, err)
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestAdminUserDetailEndpoints(t *testing.T) {
	a, kc := migratedAdminApp(t)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	t0 := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	seedLedger(t, a, t0)
	get := func(path string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.AddCookie(sessionCookie(t, a, liveSession("admin")))
		rec := httptest.NewRecorder()
		newMux(a).ServeHTTP(rec, r)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	code, body := get("/api/admin/users/alice")
	if code != 200 {
		t.Fatalf("detail: %d %v", code, body)
	}
	user := body["user"].(map[string]any)
	// alice has no Keycloak account in the fake directory: a historical
	// subject, named from the ledger.
	if user["deleted"] != true || user["username"] != "alice" || body["coverage"] != "pat_only" || body["bucket"] != "day" {
		t.Errorf("detail = %v", body)
	}
	if code, body := get("/api/admin/users/alice?model=other"); code != 200 || body["totals"].(map[string]any)["requests"].(float64) != 0 ||
		len(body["models"].([]any)) != 1 {
		t.Errorf("model filter: %d %v", code, body["totals"])
	}
	if code, _ := get("/api/admin/users/alice?from=2020-01-01T00:00:00Z"); code != 400 {
		t.Errorf("range over 180 days: %d", code)
	}
	_, page := get("/api/admin/users/alice/requests?limit=2")
	if len(page["requests"].([]any)) != 2 || page["next_before_id"] == "" {
		t.Errorf("requests page = %v", page)
	}
	_, rest := get("/api/admin/users/alice/requests?limit=2&before_id=" + url.QueryEscape(page["next_before_id"].(string)))
	if len(rest["requests"].([]any)) != 2 {
		t.Errorf("second page = %v", rest)
	}
	_, sessions := get("/api/admin/users/alice/sessions")
	if sessions["total"].(float64) != 2 {
		t.Errorf("sessions = %v", sessions)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/admin/users/alice/requests", nil)
	r.AddCookie(sessionCookie(t, a, liveSession("bob")))
	rec := httptest.NewRecorder()
	newMux(a).ServeHTTP(rec, r)
	if rec.Code != 403 {
		t.Errorf("non-admin got %d", rec.Code)
	}
}

func TestAdminOverviewDegradesWithoutClusterOrPrometheus(t *testing.T) {
	a, kc := migratedAdminApp(t)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	seedLedger(t, a, time.Now().UTC().Add(-time.Hour))
	r := httptest.NewRequest(http.MethodGet, "/api/admin/overview?hours=24", nil)
	r.AddCookie(sessionCookie(t, a, liveSession("admin")))
	rec := httptest.NewRecorder()
	newMux(a).ServeHTTP(rec, r)
	var body struct {
		Totals usageTotals       `json:"totals"`
		Errors map[string]string `json:"errors"`
		Top    []topUser         `json:"top_users"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if body.Totals.Requests != 5 || body.Totals.ActiveUsers != 1 || len(body.Top) != 1 || body.Errors["kubernetes"] == "" {
		t.Errorf("overview = %+v", body)
	}
	if !strings.Contains(body.Errors["prometheus/load1"], "not configured") {
		t.Errorf("errors = %v", body.Errors)
	}
}
