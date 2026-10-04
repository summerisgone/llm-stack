package main

import (
	"context"
	"crypto/hmac"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Administration console API (docs/adr/0022). Every /api/admin/ handler
// starts with adminGuard; every state-changing one goes through adminMutate
// so its audit event commits with the change.

type adminActor struct {
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Name    string `json:"name"`
}

// apiErr is a rejection a handler wants reported to the client as is and
// audited as a denied attempt (validation failure, revision conflict).
type apiErr struct {
	status  int
	code    string
	message string
}

func (e *apiErr) Error() string { return e.message }

func writeAdminError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func isMutation(r *http.Request) bool {
	return r.Method != http.MethodGet && r.Method != http.MethodHead
}

// auditSource labels where a request came from. It is only a label; the
// actor is always taken from the verified session.
func auditSource(r *http.Request) string {
	if r.Header.Get("X-Admin-Source") == "ui" {
		return "ui"
	}
	return "api"
}

// adminGuard authorizes an admin API request: 401 without a session, 403
// for a non-admin or a failed CSRF check, 503 when Keycloak cannot confirm
// the current membership. Mutations always re-check membership; reads may
// use a check up to adminMembershipTTL old. Rejected mutations are audited.
func (a *app) adminGuard(w http.ResponseWriter, r *http.Request) (adminActor, string, bool) {
	requestID, _ := randomURL(12)
	w.Header().Set("X-Request-Id", requestID)
	mutating := isMutation(r)
	var actor adminActor
	deny := func(status int, code, message string) (adminActor, string, bool) {
		if mutating {
			a.recordAudit(r.Context(), a.db, auditEvent{
				RequestID: requestID, Actor: actor, Action: r.Method + " " + r.URL.Path,
				Source: auditSource(r), Outcome: "denied", ErrorCode: code, ErrorMessage: message,
			})
		}
		writeAdminError(w, status, code, message)
		return adminActor{}, "", false
	}
	s, ok := a.currentSession(r)
	if !ok {
		return deny(http.StatusUnauthorized, "unauthenticated", "SSO login required")
	}
	actor = adminActor{Issuer: s.Issuer, Subject: s.Subject, Name: s.Username}
	if s.Issuer != a.cfg.issuer {
		return deny(http.StatusForbidden, "forbidden", "administrator role required")
	}
	if mutating && !a.validCSRF(r) {
		return deny(http.StatusForbidden, "csrf_failed", "CSRF check failed")
	}
	if a.directory == nil {
		return deny(http.StatusServiceUnavailable, "directory_unavailable", "user directory is not configured")
	}
	admin, err := a.directory.isAdmin(r.Context(), s.Subject, mutating)
	if err != nil {
		log.Printf("admin check for %s: %v", s.Subject, err)
		return deny(http.StatusServiceUnavailable, "directory_unavailable", "cannot confirm administrator role")
	}
	if !admin {
		return deny(http.StatusForbidden, "forbidden", "administrator role required")
	}
	return actor, requestID, true
}

// csrfToken is bound to the current session cookie, so it changes on every
// login and needs no server-side state. Empty without a session.
func (a *app) csrfToken(r *http.Request) string {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return ""
	}
	return a.sign("csrf." + c.Value)
}

// validCSRF requires the session-bound token in X-CSRF-Token and a
// same-origin request.
func (a *app) validCSRF(r *http.Request) bool {
	want := a.csrfToken(r)
	got := r.Header.Get("X-CSRF-Token")
	return want != "" && hmac.Equal([]byte(got), []byte(want)) && a.sameOrigin(r)
}

func (a *app) sameOrigin(r *http.Request) bool {
	if origin := r.Header.Get("Origin"); origin != "" {
		return origin == a.cfg.publicOrigin
	}
	return r.Header.Get("Sec-Fetch-Site") == "same-origin"
}

// sessionInfo tells the dashboard who is signed in and which sections to
// show. Hiding "Administration" is presentation only; adminGuard decides.
func (a *app) sessionInfo(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	capabilities := []string{}
	if a.directory != nil && s.Issuer == a.cfg.issuer {
		admin, err := a.directory.isAdmin(r.Context(), s.Subject, false)
		if err != nil {
			log.Printf("admin capability for %s: %v", s.Subject, err)
		}
		if admin {
			capabilities = append(capabilities, "admin")
		}
	}
	writeJSON(w, 200, map[string]any{"subject": s.Subject, "username": s.Username, "capabilities": capabilities})
}

type auditEvent struct {
	RequestID         string
	OperationID       string
	ParentOperationID string
	Actor             adminActor
	ServiceIdentity   string
	Action            string
	TargetType        string
	TargetID          string
	Source            string
	Reason            string
	ExpectedRevision  *int64
	AppliedRevision   *int64
	// Before and After must already be sanitized: no tokens, cookies,
	// credentials, prompts or secret-bearing values.
	Before       any
	After        any
	Outcome      string
	ErrorCode    string
	ErrorMessage string
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

func jsonOrNil(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

func insertAudit(ctx context.Context, db execer, ev auditEvent) error {
	before, err := jsonOrNil(ev.Before)
	if err != nil {
		return err
	}
	after, err := jsonOrNil(ev.After)
	if err != nil {
		return err
	}
	if len(ev.ErrorMessage) > 500 {
		ev.ErrorMessage = ev.ErrorMessage[:500]
	}
	_, err = db.Exec(ctx, `INSERT INTO admin_audit_events
		(request_id, operation_id, parent_operation_id, actor_issuer, actor_subject, actor_name, service_identity,
		 action, target_type, target_id, source, reason, expected_revision, applied_revision,
		 before_value, after_value, outcome, error_code, error_message)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)`,
		ev.RequestID, ev.OperationID, ev.ParentOperationID, ev.Actor.Issuer, ev.Actor.Subject, ev.Actor.Name, ev.ServiceIdentity,
		ev.Action, ev.TargetType, ev.TargetID, ev.Source, ev.Reason, ev.ExpectedRevision, ev.AppliedRevision,
		before, after, ev.Outcome, ev.ErrorCode, ev.ErrorMessage)
	return err
}

// recordAudit writes an event outside any change, for rejected attempts.
// Its failure cannot change the already-negative answer, so it only logs.
func (a *app) recordAudit(ctx context.Context, db execer, ev auditEvent) {
	if a.db == nil {
		return
	}
	if err := insertAudit(ctx, db, ev); err != nil {
		log.Printf("audit %s %s: %v", ev.Action, ev.Outcome, err)
	}
}

// adminMutate runs change and its succeeded audit event in one transaction
// and answers 200 with the new value. An *apiErr from change is answered
// as is and audited as denied; any other failure, including the audit
// insert itself, rolls back and leaves no success record.
func (a *app) adminMutate(w http.ResponseWriter, r *http.Request, actor adminActor, requestID string, ev auditEvent,
	change func(ctx context.Context, tx pgx.Tx) (before, after any, err error)) {
	ctx := r.Context()
	ev.RequestID, ev.Actor, ev.Source = requestID, actor, auditSource(r)
	tx, err := a.db.Begin(ctx)
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
		return
	}
	defer tx.Rollback(ctx)
	before, after, err := change(ctx, tx)
	var rejected *apiErr
	if errors.As(err, &rejected) {
		_ = tx.Rollback(ctx)
		ev.Outcome, ev.ErrorCode, ev.ErrorMessage = "denied", rejected.code, rejected.message
		a.recordAudit(ctx, a.db, ev)
		writeAdminError(w, rejected.status, rejected.code, rejected.message)
		return
	}
	if err != nil {
		_ = tx.Rollback(ctx)
		log.Printf("admin %s: %v", ev.Action, err)
		ev.Outcome, ev.ErrorCode, ev.ErrorMessage = "failed", "internal", "change failed"
		a.recordAudit(ctx, a.db, ev)
		writeAdminError(w, http.StatusInternalServerError, "internal", "change failed")
		return
	}
	ev.Before, ev.After, ev.Outcome = before, after, "succeeded"
	if err := insertAudit(ctx, tx, ev); err != nil {
		log.Printf("audit %s: %v", ev.Action, err)
		writeAdminError(w, http.StatusServiceUnavailable, "audit_unavailable", "audit log unavailable; nothing was changed")
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable; nothing was changed")
		return
	}
	writeJSON(w, 200, after)
}

type adminUserRow struct {
	Subject        string     `json:"subject"`
	Username       string     `json:"username"`
	Email          string     `json:"email"`
	Name           string     `json:"name"`
	Enabled        bool       `json:"enabled"`
	Roles          []string   `json:"roles"`
	LastActivityAt *time.Time `json:"last_activity_at"`
	Requests       int64      `json:"requests"`
	Tokens         int64      `json:"tokens"`
	CostAmount     float64    `json:"cost_amount"`
}

// adminUsers lists the Keycloak directory page by page, users with no
// requests included, joined with their PAT-path usage over `days`.
func (a *app) adminUsers(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	ctx := r.Context()
	size := clampIntQuery(r, "size", 25, 1, 100)
	page := clampIntQuery(r, "page", 0, 0, 1<<20)
	days := clampIntQuery(r, "days", 30, 1, 365)
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	users, total, err := a.directory.listUsers(ctx, search, page*size, size)
	if err != nil {
		log.Printf("admin users: %v", err)
		writeAdminError(w, http.StatusServiceUnavailable, "directory_unavailable", "user directory unavailable")
		return
	}
	rows := make([]adminUserRow, len(users))
	subjects := make([]string, len(users))
	index := map[string]int{}
	var wg sync.WaitGroup
	var roleErr error
	var roleMu sync.Mutex
	for i, u := range users {
		rows[i] = adminUserRow{Subject: u.ID, Username: u.Username, Email: u.Email, Name: strings.TrimSpace(u.FirstName + " " + u.LastName), Enabled: u.Enabled, Roles: []string{}}
		subjects[i] = u.ID
		index[u.ID] = i
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			roles, err := a.directory.realmRoles(ctx, id)
			roleMu.Lock()
			defer roleMu.Unlock()
			if err != nil {
				roleErr = err
				return
			}
			for _, role := range roles {
				if role == "ai-user" || role == "ai-admin" {
					rows[i].Roles = append(rows[i].Roles, role)
				}
			}
		}(i, u.ID)
	}
	wg.Wait()
	if roleErr != nil {
		log.Printf("admin users roles: %v", roleErr)
		writeAdminError(w, http.StatusServiceUnavailable, "directory_unavailable", "user directory unavailable")
		return
	}
	usage, err := a.db.Query(ctx, `
		SELECT owner_subject, max(created_at),
		       count(*) FILTER (WHERE created_at >= now() - make_interval(days => $3)),
		       COALESCE(SUM(prompt_tokens + completion_tokens) FILTER (WHERE created_at >= now() - make_interval(days => $3)), 0),
		       COALESCE(SUM(cost_amount) FILTER (WHERE created_at >= now() - make_interval(days => $3)), 0)
		FROM qos_events
		WHERE owner_issuer = $1 AND owner_subject = ANY($2)
		GROUP BY owner_subject`, a.cfg.issuer, subjects, days)
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
		return
	}
	defer usage.Close()
	for usage.Next() {
		var subject string
		var last time.Time
		var requests, tokens int64
		var cost float64
		if err := usage.Scan(&subject, &last, &requests, &tokens, &cost); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "internal", "database error")
			return
		}
		row := &rows[index[subject]]
		row.LastActivityAt, row.Requests, row.Tokens, row.CostAmount = &last, requests, tokens, cost
	}
	resp := a.coverage(time.Now().AddDate(0, 0, -days))
	resp["users"], resp["total"], resp["page"], resp["size"], resp["days"] = rows, total, page, size, days
	resp["currency"] = a.pricing.Currency
	writeJSON(w, 200, resp)
}

type auditRecord struct {
	ID                int64           `json:"id"`
	OccurredAt        time.Time       `json:"occurred_at"`
	RequestID         string          `json:"request_id"`
	OperationID       string          `json:"operation_id"`
	ParentOperationID string          `json:"parent_operation_id"`
	Actor             adminActor      `json:"actor"`
	ServiceIdentity   string          `json:"service_identity"`
	Action            string          `json:"action"`
	TargetType        string          `json:"target_type"`
	TargetID          string          `json:"target_id"`
	Source            string          `json:"source"`
	Reason            string          `json:"reason"`
	ExpectedRevision  *int64          `json:"expected_revision"`
	AppliedRevision   *int64          `json:"applied_revision"`
	Before            json.RawMessage `json:"before"`
	After             json.RawMessage `json:"after"`
	Outcome           string          `json:"outcome"`
	ErrorCode         string          `json:"error_code"`
	ErrorMessage      string          `json:"error_message"`
}

const auditMaxRange = 366 * 24 * time.Hour

// adminAudit pages the journal newest first. The time range defaults to the
// last 30 days; an explicit range is capped at auditMaxRange. before_id
// continues a page.
func (a *app) adminAudit(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	q := r.URL.Query()
	// Without explicit bounds the window is the database's last 30 days, so
	// events stamped by the database clock are never cut off by skew.
	where := []string{"occurred_at >= now() - interval '30 days'"}
	var args []any
	add := func(cond string, v any) {
		args = append(args, v)
		where = append(where, strings.Replace(cond, "?", "$"+strconv.Itoa(len(args)), 1))
	}
	times := map[string]time.Time{}
	for _, name := range []string{"from", "to"} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.RFC3339, v)
			if err != nil {
				writeAdminError(w, 400, "invalid_time", name+" must be RFC 3339")
				return
			}
			times[name] = t
		}
	}
	from, hasFrom := times["from"]
	to, hasTo := times["to"]
	if hasFrom {
		where = where[1:]
		end := to
		if !hasTo {
			end = time.Now()
		}
		if !from.Before(end) || end.Sub(from) > auditMaxRange {
			writeAdminError(w, 400, "invalid_range", "time range must be positive and at most 366 days")
			return
		}
		add("occurred_at >= ?", from)
	}
	if hasTo {
		add("occurred_at < ?", to)
	}
	limit := clampIntQuery(r, "limit", 50, 1, 200)
	for param, column := range map[string]string{"actor": "actor_subject", "action": "action", "target_type": "target_type", "target_id": "target_id", "outcome": "outcome", "operation_id": "operation_id"} {
		if v := q.Get(param); v != "" {
			add(column+" = ?", v)
		}
	}
	if v := q.Get("before_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			writeAdminError(w, 400, "invalid_cursor", "before_id must be an integer")
			return
		}
		add("id < ?", id)
	}
	args = append(args, limit+1)
	rows, err := a.db.Query(r.Context(), `
		SELECT id, occurred_at, request_id, operation_id, parent_operation_id, actor_issuer, actor_subject, actor_name,
		       service_identity, action, target_type, target_id, source, reason, expected_revision, applied_revision,
		       before_value, after_value, outcome, error_code, error_message
		FROM admin_audit_events WHERE `+strings.Join(where, " AND ")+`
		ORDER BY id DESC LIMIT $`+strconv.Itoa(len(args)), args...)
	if err != nil {
		writeAdminError(w, http.StatusServiceUnavailable, "database_unavailable", "database unavailable")
		return
	}
	defer rows.Close()
	events := []auditRecord{}
	for rows.Next() {
		var e auditRecord
		if err := rows.Scan(&e.ID, &e.OccurredAt, &e.RequestID, &e.OperationID, &e.ParentOperationID, &e.Actor.Issuer, &e.Actor.Subject, &e.Actor.Name,
			&e.ServiceIdentity, &e.Action, &e.TargetType, &e.TargetID, &e.Source, &e.Reason, &e.ExpectedRevision, &e.AppliedRevision,
			&e.Before, &e.After, &e.Outcome, &e.ErrorCode, &e.ErrorMessage); err != nil {
			writeAdminError(w, http.StatusInternalServerError, "internal", "database error")
			return
		}
		events = append(events, e)
	}
	var next string
	if len(events) > limit {
		events = events[:limit]
		next = strconv.FormatInt(events[limit-1].ID, 10)
	}
	writeJSON(w, 200, map[string]any{"events": events, "next_before_id": next})
}

// publicOrigin is scheme://host of the public redirect URL, the only origin
// allowed to send state-changing dashboard requests.
func publicOrigin(redirectURL string) string {
	u, err := url.Parse(redirectURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}
