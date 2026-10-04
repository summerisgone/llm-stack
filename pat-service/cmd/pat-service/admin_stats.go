package main

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Per-user statistics from the request ledger, with the definitions of
// docs/adr/0022 section 6. Times are stored and returned in UTC; the UI
// states the timezone it displays.

// statsMaxRange matches the detailed request retention (ADR 0022 section 6).
const statsMaxRange = 180 * 24 * time.Hour

type statsFilter struct {
	from, to time.Time
	model    string
	source   string
}

func parseStatsFilter(r *http.Request, def time.Duration) (statsFilter, *apiErr) {
	q := r.URL.Query()
	f := statsFilter{to: time.Now().UTC(), model: q.Get("model"), source: q.Get("source")}
	f.from = f.to.Add(-def)
	for name, dst := range map[string]*time.Time{"from": &f.from, "to": &f.to} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				return f, &apiErr{400, "invalid_time", name + " must be RFC 3339"}
			}
			*dst = t.UTC()
		}
	}
	if !f.from.Before(f.to) || f.to.Sub(f.from) > statsMaxRange {
		return f, &apiErr{400, "invalid_range", "time range must be positive and at most 180 days"}
	}
	return f, nil
}

// where returns the ledger condition for one user (or every user when
// subject is empty) and its arguments, starting at $1.
func (f statsFilter) where(issuer, subject string) (string, []any) {
	conds := []string{"owner_issuer = $1", "created_at >= $2", "created_at < $3"}
	args := []any{issuer, f.from, f.to}
	add := func(cond string, v any) {
		args = append(args, v)
		conds = append(conds, strings.Replace(cond, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	if subject != "" {
		add("owner_subject = ?", subject)
	}
	if f.model != "" {
		add("model = ?", f.model)
	}
	if f.source != "" {
		add("source = ?", f.source)
	}
	return strings.Join(conds, " AND "), args
}

// bucketFor picks the timeline granularity: hours up to two days, days
// beyond.
func bucketFor(f statsFilter) string {
	if f.to.Sub(f.from) <= 48*time.Hour {
		return "hour"
	}
	return "day"
}

type usageTotals struct {
	Requests          int64            `json:"requests"`
	Outcomes          map[string]int64 `json:"outcomes"`
	PromptTokens      int64            `json:"prompt_tokens"`
	CachedTokens      int64            `json:"cached_tokens"`
	CompletionTokens  int64            `json:"completion_tokens"`
	ActualTokens      int64            `json:"actual_tokens"`
	UnknownUsage      int64            `json:"requests_without_usage"`
	CostAmount        float64          `json:"cost_amount"`
	Sessions          int64            `json:"sessions"`
	RequestsNoSession int64            `json:"requests_without_session"`
	ActiveUsers       int64            `json:"active_users"`
}

func (a *app) queryTotals(ctx context.Context, where string, args []any) (usageTotals, error) {
	t := usageTotals{Outcomes: map[string]int64{}}
	err := a.db.QueryRow(ctx, `
		SELECT count(*), COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(cached_tokens),0), COALESCE(SUM(completion_tokens),0),
		       COALESCE(SUM(prompt_tokens + completion_tokens) FILTER (WHERE usage_quality = 'actual'),0),
		       count(*) FILTER (WHERE usage_quality <> 'actual'), COALESCE(SUM(cost_amount),0),
		       count(DISTINCT session_id) FILTER (WHERE session_id <> ''), count(*) FILTER (WHERE session_id = ''),
		       count(DISTINCT owner_subject)
		FROM qos_events WHERE `+where, args...).Scan(&t.Requests, &t.PromptTokens, &t.CachedTokens, &t.CompletionTokens,
		&t.ActualTokens, &t.UnknownUsage, &t.CostAmount, &t.Sessions, &t.RequestsNoSession, &t.ActiveUsers)
	if err != nil {
		return t, err
	}
	rows, err := a.db.Query(ctx, `SELECT outcome, count(*) FROM qos_events WHERE `+where+` GROUP BY outcome`, args...)
	if err != nil {
		return t, err
	}
	defer rows.Close()
	for rows.Next() {
		var outcome string
		var n int64
		if err := rows.Scan(&outcome, &n); err != nil {
			return t, err
		}
		t.Outcomes[outcome] = n
	}
	return t, rows.Err()
}

type latencyStats struct {
	TTFTp50       *float64         `json:"ttft_p50_seconds"`
	TTFTp95       *float64         `json:"ttft_p95_seconds"`
	TTFTSamples   int64            `json:"ttft_samples"`
	TPOTp50       *float64         `json:"tpot_p50_seconds"`
	TPOTp95       *float64         `json:"tpot_p95_seconds"`
	TPOTSamples   int64            `json:"tpot_samples"`
	OutputRate    *float64         `json:"output_tokens_per_second"`
	RateSamples   int64            `json:"rate_samples"`
	NotMeasurable map[string]int64 `json:"not_measurable"`
}

// queryLatency computes TTFT (receipt to first output), TPOT ((last-first)/
// (output tokens-1), complete streams with 2+ tokens) and the end-to-end
// output rate (sum of tokens over sum of receipt-to-last-output times,
// never an average of per-request rates) from per-request samples only.
func (a *app) queryLatency(ctx context.Context, where string, args []any) (latencyStats, error) {
	l := latencyStats{}
	var untimed, nonStreaming, noOutput int64
	err := a.db.QueryRow(ctx, `
		WITH s AS (
		  SELECT received_at, streaming,
		    EXTRACT(EPOCH FROM first_output_at - received_at) AS ttft,
		    CASE WHEN streaming AND outcome = 'ok' AND usage_quality = 'actual' AND completion_tokens > 1 AND last_output_at > first_output_at
		         THEN EXTRACT(EPOCH FROM last_output_at - first_output_at) / (completion_tokens - 1) END AS tpot,
		    CASE WHEN outcome = 'ok' AND usage_quality = 'actual' AND completion_tokens > 0 AND last_output_at > received_at
		         THEN completion_tokens END AS rate_tokens,
		    CASE WHEN outcome = 'ok' AND usage_quality = 'actual' AND completion_tokens > 0 AND last_output_at > received_at
		         THEN EXTRACT(EPOCH FROM last_output_at - received_at) END AS rate_seconds
		  FROM qos_events WHERE `+where+`)
		SELECT percentile_cont(0.5) WITHIN GROUP (ORDER BY ttft), percentile_cont(0.95) WITHIN GROUP (ORDER BY ttft), count(ttft),
		       percentile_cont(0.5) WITHIN GROUP (ORDER BY tpot), percentile_cont(0.95) WITHIN GROUP (ORDER BY tpot), count(tpot),
		       SUM(rate_tokens) / NULLIF(SUM(rate_seconds), 0), count(rate_seconds),
		       count(*) FILTER (WHERE received_at IS NULL),
		       count(*) FILTER (WHERE received_at IS NOT NULL AND streaming IS NOT TRUE),
		       count(*) FILTER (WHERE streaming AND ttft IS NULL)
		FROM s`, args...).Scan(&l.TTFTp50, &l.TTFTp95, &l.TTFTSamples, &l.TPOTp50, &l.TPOTp95, &l.TPOTSamples,
		&l.OutputRate, &l.RateSamples, &untimed, &nonStreaming, &noOutput)
	l.NotMeasurable = map[string]int64{"recorded_before_timing": untimed, "not_streaming": nonStreaming, "no_output": noOutput}
	return l, err
}

type usageTime struct {
	ActiveSeconds      float64 `json:"active_seconds"`
	SummedSeconds      float64 `json:"summed_request_seconds"`
	SessionSpanSeconds float64 `json:"session_span_seconds"`
	TimedRequests      int64   `json:"timed_requests"`
}

// queryUsageTime: active time is the union of request intervals clipped to
// the range, so concurrent requests are not counted twice; the summed
// request time and the sessions' wall-clock span are reported beside it.
func (a *app) queryUsageTime(ctx context.Context, where string, args []any) (usageTime, error) {
	u := usageTime{}
	err := a.db.QueryRow(ctx, `
		WITH iv AS (
		  SELECT GREATEST(received_at, $2) AS s, LEAST(finished_at, $3) AS e
		  FROM qos_events WHERE `+where+` AND received_at IS NOT NULL AND finished_at > received_at),
		ordered AS (
		  SELECT s, e, MAX(e) OVER (ORDER BY s, e ROWS BETWEEN UNBOUNDED PRECEDING AND 1 PRECEDING) AS prev_end
		  FROM iv WHERE e > s),
		grouped AS (
		  SELECT s, e, SUM(CASE WHEN prev_end IS NULL OR s > prev_end THEN 1 ELSE 0 END) OVER (ORDER BY s, e) AS g
		  FROM ordered),
		islands AS (SELECT MIN(s) AS s, MAX(e) AS e FROM grouped GROUP BY g)
		SELECT COALESCE((SELECT SUM(EXTRACT(EPOCH FROM e - s)) FROM islands), 0),
		       COALESCE((SELECT SUM(EXTRACT(EPOCH FROM e - s)) FROM iv WHERE e > s), 0),
		       (SELECT count(*) FROM iv)`, args...).Scan(&u.ActiveSeconds, &u.SummedSeconds, &u.TimedRequests)
	if err != nil {
		return u, err
	}
	err = a.db.QueryRow(ctx, `
		SELECT COALESCE(SUM(span), 0) FROM (
		  SELECT EXTRACT(EPOCH FROM MAX(COALESCE(finished_at, created_at)) - MIN(COALESCE(received_at, created_at))) AS span
		  FROM qos_events WHERE `+where+` AND session_id <> '' GROUP BY session_id) x`, args...).Scan(&u.SessionSpanSeconds)
	return u, err
}

type timelinePoint struct {
	At               time.Time `json:"at"`
	Requests         int64     `json:"requests"`
	Failed           int64     `json:"failed"`
	PromptTokens     int64     `json:"prompt_tokens"`
	CachedTokens     int64     `json:"cached_tokens"`
	CompletionTokens int64     `json:"completion_tokens"`
}

func (a *app) queryTimeline(ctx context.Context, bucket, where string, args []any) ([]timelinePoint, error) {
	rows, err := a.db.Query(ctx, `
		SELECT date_trunc('`+bucket+`', created_at AT TIME ZONE 'UTC') AS b, count(*), count(*) FILTER (WHERE outcome <> 'ok'),
		       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(cached_tokens),0), COALESCE(SUM(completion_tokens),0)
		FROM qos_events WHERE `+where+` GROUP BY b ORDER BY b`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []timelinePoint{}
	for rows.Next() {
		var p timelinePoint
		if err := rows.Scan(&p.At, &p.Requests, &p.Failed, &p.PromptTokens, &p.CachedTokens, &p.CompletionTokens); err != nil {
			return nil, err
		}
		p.At = time.Date(p.At.Year(), p.At.Month(), p.At.Day(), p.At.Hour(), 0, 0, 0, time.UTC)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (a *app) distinctValues(ctx context.Context, column, where string, args []any) ([]string, error) {
	rows, err := a.db.Query(ctx, `SELECT DISTINCT `+column+` FROM qos_events WHERE `+where+` ORDER BY 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

type userIdentity struct {
	Subject  string   `json:"subject"`
	Username string   `json:"username"`
	Email    string   `json:"email"`
	Name     string   `json:"name"`
	Enabled  bool     `json:"enabled"`
	Roles    []string `json:"roles"`
	// Deleted marks a subject that only exists in history: no Keycloak
	// account has it any more.
	Deleted bool `json:"deleted"`
}

func (a *app) userIdentity(ctx context.Context, subject string) (userIdentity, error) {
	id := userIdentity{Subject: subject, Roles: []string{}}
	var u directoryUser
	found, err := a.directory.get(ctx, "/users/"+url.PathEscape(subject), nil, &u)
	if err != nil {
		return id, err
	}
	if !found {
		id.Deleted = true
		_ = a.db.QueryRow(ctx, `SELECT owner_name FROM qos_events WHERE owner_issuer = $1 AND owner_subject = $2 ORDER BY id DESC LIMIT 1`, a.cfg.issuer, subject).Scan(&id.Username)
		return id, nil
	}
	id.Username, id.Email, id.Name, id.Enabled = u.Username, u.Email, strings.TrimSpace(u.FirstName+" "+u.LastName), u.Enabled
	roles, err := a.directory.realmRoles(ctx, subject)
	if err != nil {
		return id, err
	}
	for _, r := range roles {
		if r == "ai-user" || r == "ai-admin" {
			id.Roles = append(id.Roles, r)
		}
	}
	return id, nil
}

// adminUserDetail is the User details screen: identity, totals, latency,
// usage time, timeline and the filter choices for the range.
func (a *app) adminUserDetail(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	ctx := r.Context()
	subject := r.PathValue("subject")
	f, bad := parseStatsFilter(r, 7*24*time.Hour)
	if bad != nil {
		writeAdminError(w, bad.status, bad.code, bad.message)
		return
	}
	identity, err := a.userIdentity(ctx, subject)
	if err != nil {
		log.Printf("admin user %s: %v", subject, err)
		writeAdminError(w, http.StatusServiceUnavailable, "directory_unavailable", "user directory unavailable")
		return
	}
	where, args := f.where(a.cfg.issuer, subject)
	unfiltered, unfilteredArgs := statsFilter{from: f.from, to: f.to}.where(a.cfg.issuer, subject)
	resp := a.coverage(f.from)
	resp["user"], resp["from"], resp["to"], resp["bucket"], resp["currency"] = identity, f.from, f.to, bucketFor(f), a.pricing.Currency
	fail := func(err error) {
		log.Printf("admin user %s stats: %v", subject, err)
		writeAdminError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
	}
	totals, err := a.queryTotals(ctx, where, args)
	if err != nil {
		fail(err)
		return
	}
	latency, err := a.queryLatency(ctx, where, args)
	if err != nil {
		fail(err)
		return
	}
	usage, err := a.queryUsageTime(ctx, where, args)
	if err != nil {
		fail(err)
		return
	}
	timeline, err := a.queryTimeline(ctx, bucketFor(f), where, args)
	if err != nil {
		fail(err)
		return
	}
	models, err := a.distinctValues(ctx, "model", unfiltered, unfilteredArgs)
	if err != nil {
		fail(err)
		return
	}
	sources, err := a.distinctValues(ctx, "source", unfiltered, unfilteredArgs)
	if err != nil {
		fail(err)
		return
	}
	var historyFrom, timedFrom *time.Time
	if err := a.db.QueryRow(ctx, `SELECT MIN(created_at), MIN(received_at) FROM qos_events WHERE owner_issuer = $1 AND owner_subject = $2`,
		a.cfg.issuer, subject).Scan(&historyFrom, &timedFrom); err != nil {
		fail(err)
		return
	}
	resp["totals"], resp["latency"], resp["usage_time"], resp["timeline"] = totals, latency, usage, timeline
	resp["models"], resp["sources"], resp["history_from"], resp["timing_from"] = models, sources, historyFrom, timedFrom
	writeJSON(w, 200, resp)
}

type requestRow struct {
	ID               int64      `json:"id"`
	FinishedAt       time.Time  `json:"recorded_at"`
	Model            string     `json:"model"`
	Source           string     `json:"source"`
	TokenName        string     `json:"token_name"`
	SessionID        string     `json:"session_id"`
	Outcome          string     `json:"outcome"`
	HTTPStatus       *int       `json:"http_status"`
	Streaming        *bool      `json:"streaming"`
	UsageQuality     string     `json:"usage_quality"`
	PromptTokens     int64      `json:"prompt_tokens"`
	CachedTokens     int64      `json:"cached_tokens"`
	CompletionTokens int64      `json:"completion_tokens"`
	CostAmount       float64    `json:"cost_amount"`
	TTFTSeconds      *float64   `json:"ttft_seconds"`
	DurationSeconds  *float64   `json:"duration_seconds"`
	ReceivedAt       *time.Time `json:"received_at"`
}

// adminUserRequests pages the user's requests, newest first.
func (a *app) adminUserRequests(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	f, bad := parseStatsFilter(r, 7*24*time.Hour)
	if bad != nil {
		writeAdminError(w, bad.status, bad.code, bad.message)
		return
	}
	where, args := f.where(a.cfg.issuer, r.PathValue("subject"))
	if v := r.URL.Query().Get("outcome"); v != "" {
		args = append(args, v)
		where += " AND outcome = $" + strconv.Itoa(len(args))
	}
	if v := r.URL.Query().Get("before_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeAdminError(w, 400, "invalid_cursor", "before_id must be an integer")
			return
		}
		args = append(args, id)
		where += " AND id < $" + strconv.Itoa(len(args))
	}
	limit := clampIntQuery(r, "limit", 50, 1, 200)
	args = append(args, limit+1)
	rows, err := a.db.Query(r.Context(), `
		SELECT id, created_at, model, source, token_name, session_id, outcome, http_status, streaming, usage_quality,
		       prompt_tokens, cached_tokens, completion_tokens, cost_amount,
		       EXTRACT(EPOCH FROM first_output_at - received_at)::float8, EXTRACT(EPOCH FROM finished_at - received_at)::float8, received_at
		FROM qos_events WHERE `+where+` ORDER BY id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
		return
	}
	defer rows.Close()
	out := []requestRow{}
	for rows.Next() {
		var q requestRow
		if err := rows.Scan(&q.ID, &q.FinishedAt, &q.Model, &q.Source, &q.TokenName, &q.SessionID, &q.Outcome, &q.HTTPStatus, &q.Streaming, &q.UsageQuality,
			&q.PromptTokens, &q.CachedTokens, &q.CompletionTokens, &q.CostAmount, &q.TTFTSeconds, &q.DurationSeconds, &q.ReceivedAt); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "internal", "database error")
			return
		}
		out = append(out, q)
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = strconv.FormatInt(out[limit-1].ID, 10)
	}
	writeJSON(w, 200, map[string]any{"requests": out, "next_before_id": next, "currency": a.pricing.Currency})
}

// adminUserSessions lists the user's inference sessions active in the range.
func (a *app) adminUserSessions(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	f, bad := parseStatsFilter(r, 7*24*time.Hour)
	if bad != nil {
		writeAdminError(w, bad.status, bad.code, bad.message)
		return
	}
	where, args := f.where(a.cfg.issuer, r.PathValue("subject"))
	where += " AND session_id <> ''"
	limit := clampIntQuery(r, "limit", 20, 1, 200)
	offset := clampIntQuery(r, "offset", 0, 0, 1<<30)
	var total int64
	if err := a.db.QueryRow(r.Context(), `SELECT count(DISTINCT session_id) FROM qos_events WHERE `+where, args...).Scan(&total); err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
		return
	}
	n := len(args)
	rows, err := a.db.Query(r.Context(), `
		SELECT session_id, max(model), max(token_name), min(COALESCE(received_at, created_at)), max(COALESCE(finished_at, created_at)), count(*),
		       COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cost_amount),0)
		FROM qos_events WHERE `+where+`
		GROUP BY session_id ORDER BY max(created_at) DESC LIMIT $`+strconv.Itoa(n+1)+` OFFSET $`+strconv.Itoa(n+2), append(args, limit, offset)...)
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
		return
	}
	defer rows.Close()
	out := []sessionUsage{}
	for rows.Next() {
		var started, ended time.Time
		var d sessionUsage
		if err := rows.Scan(&d.SessionID, &d.Model, &d.TokenName, &started, &ended, &d.Steps, &d.PromptTokens, &d.CompletionTokens, &d.CostAmount); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "internal", "database error")
			return
		}
		d.StartedAt, d.EndedAt = started.Format(time.RFC3339), ended.Format(time.RFC3339)
		out = append(out, d)
	}
	writeJSON(w, 200, map[string]any{"sessions": out, "total": total, "currency": a.pricing.Currency})
}

type topUser struct {
	Subject  string  `json:"subject"`
	Name     string  `json:"name"`
	Requests int64   `json:"requests"`
	Tokens   int64   `json:"tokens"`
	Cost     float64 `json:"cost_amount"`
}

// adminOverview summarises the selected period across users plus the
// current state of models and nodes. Every source can fail independently.
func (a *app) adminOverview(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	ctx := r.Context()
	hours := clampIntQuery(r, "hours", 24, 1, 24*30)
	f := statsFilter{to: time.Now().UTC()}
	f.from = f.to.Add(-time.Duration(hours) * time.Hour)
	where, args := f.where(a.cfg.issuer, "")
	resp := a.coverage(f.from)
	resp["from"], resp["to"], resp["bucket"], resp["currency"] = f.from, f.to, bucketFor(f), a.pricing.Currency
	// Token quotas are not enforced before ADR 0022 stage 4; 429s in
	// outcomes come from the gateway's own limits.
	resp["quota_rejections"] = nil
	errs := map[string]string{}
	if totals, err := a.queryTotals(ctx, where, args); err == nil {
		resp["totals"] = totals
	} else {
		errs["ledger"] = err.Error()
	}
	if timeline, err := a.queryTimeline(ctx, bucketFor(f), where, args); err == nil {
		resp["timeline"] = timeline
	} else {
		errs["ledger"] = err.Error()
	}
	top := []topUser{}
	if rows, err := a.db.Query(ctx, `
		SELECT owner_subject, max(owner_name), count(*), COALESCE(SUM(prompt_tokens + completion_tokens),0), COALESCE(SUM(cost_amount),0)
		FROM qos_events WHERE `+where+` GROUP BY owner_subject ORDER BY 4 DESC LIMIT 5`, args...); err == nil {
		for rows.Next() {
			var u topUser
			if rows.Scan(&u.Subject, &u.Name, &u.Requests, &u.Tokens, &u.Cost) == nil {
				top = append(top, u)
			}
		}
		rows.Close()
	} else {
		errs["ledger"] = err.Error()
	}
	resp["top_users"] = top

	snap, snapErr := a.clusterSnapshot(ctx)
	nodesResp, nodes := a.nodesAndHardware(ctx, snap, snapErr)
	for k, v := range nodesResp["errors"].(map[string]string) {
		errs[k] = v
	}
	summary := map[string]any{"total": len(nodes), "not_ready": 0}
	notReady := 0
	var hottest *sensorReading
	hottestWhere, maxLoadNode := "", ""
	var maxLoad *float64
	consider := func(where string, s sensorReading) {
		if hottest == nil || s.Celsius > hottest.Celsius {
			hottest, hottestWhere = &s, where
		}
	}
	for _, n := range nodes {
		if n.Ready == nil || !*n.Ready || len(n.Problems) > 0 {
			notReady++
		}
		if h := n.Hardware; h != nil {
			if h.NormalizedLoad != nil && (maxLoad == nil || *h.NormalizedLoad > *maxLoad) {
				maxLoad, maxLoadNode = h.NormalizedLoad, n.Name
			}
			for _, s := range h.Sensors {
				consider(n.Name, s)
			}
			for _, g := range h.GPUs {
				if g.TempCelsius != nil && g.SharedWithNode == "" {
					consider(n.Name, sensorReading{Label: "GPU " + g.Index + " " + g.Name, Celsius: *g.TempCelsius})
				}
			}
		}
	}
	summary["not_ready"] = notReady
	summary["max_normalized_load"], summary["max_load_node"] = maxLoad, maxLoadNode
	if hottest != nil {
		summary["hottest"] = map[string]any{"node": hottestWhere, "label": hottest.Label, "celsius": hottest.Celsius}
	}
	resp["nodes"] = summary

	if snapErr == nil {
		engines := snap.engines()
		models := map[string]bool{}
		for _, m := range snap.modelRoutes(a.cfg.topologyNamespace, engines, nil) {
			if m.Model != "" {
				models[m.Model] = true
			}
		}
		names := make([]string, 0, len(models))
		for m := range models {
			names = append(names, m)
		}
		sort.Strings(names)
		resp["inference"] = map[string]any{"engines": engines, "pools": snap.poolViews(engines), "models": names}
	}
	resp["errors"] = errs
	writeJSON(w, 200, resp)
}
