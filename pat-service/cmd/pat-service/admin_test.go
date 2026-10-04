package main

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

const testIssuer = "https://ai.example.com/sso/realms/ai-stack"

// fakeKeycloak serves the token, JWKS and admin user endpoints adminGuard
// and verifyJWT use.
type fakeKeycloak struct {
	srv   *httptest.Server
	key   *rsa.PrivateKey
	mu    sync.Mutex
	users map[string]fakeUser
	down  atomic.Bool
	reads atomic.Int64
}

type fakeUser struct {
	username string
	enabled  bool
	roles    []string
}

func newFakeKeycloak(t *testing.T) *fakeKeycloak {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	kc := &fakeKeycloak{key: key, users: map[string]fakeUser{}}
	const admin = "/admin/realms/ai-stack"
	kc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if kc.down.Load() {
			http.Error(w, "down", 500)
			return
		}
		switch {
		case r.URL.Path == "/realms/ai-stack/protocol/openid-connect/token":
			writeJSON(w, 200, map[string]any{"access_token": "directory-token", "expires_in": 300})
		case r.URL.Path == "/realms/ai-stack/protocol/openid-connect/certs":
			writeJSON(w, 200, map[string]any{"keys": []map[string]string{{
				"kid": "k1", "kty": "RSA",
				"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
			}}})
		case strings.HasPrefix(r.URL.Path, admin+"/users"):
			if r.Header.Get("Authorization") != "Bearer directory-token" {
				http.Error(w, "unauthorized", 401)
				return
			}
			kc.reads.Add(1)
			kc.serveUsers(w, strings.TrimPrefix(r.URL.Path, admin+"/users"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(kc.srv.Close)
	return kc
}

func (kc *fakeKeycloak) serveUsers(w http.ResponseWriter, rest string) {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	if rest == "" {
		list := []map[string]any{}
		for id, u := range kc.users {
			list = append(list, map[string]any{"id": id, "username": u.username, "enabled": u.enabled})
		}
		writeJSON(w, 200, list)
		return
	}
	if rest == "/count" {
		writeJSON(w, 200, len(kc.users))
		return
	}
	id, roles := strings.TrimPrefix(rest, "/"), false
	if before, ok := strings.CutSuffix(id, "/role-mappings/realm/composite"); ok {
		id, roles = before, true
	}
	u, ok := kc.users[id]
	if !ok {
		http.NotFound(w, nil)
		return
	}
	if roles {
		out := []map[string]string{}
		for _, role := range u.roles {
			out = append(out, map[string]string{"name": role})
		}
		writeJSON(w, 200, out)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "username": u.username, "enabled": u.enabled})
}

func (kc *fakeKeycloak) setUser(id string, u fakeUser) {
	kc.mu.Lock()
	defer kc.mu.Unlock()
	kc.users[id] = u
}

func (kc *fakeKeycloak) sign(t *testing.T, cl map[string]any) string {
	t.Helper()
	head, _ := json.Marshal(map[string]string{"alg": "RS256", "kid": "k1"})
	body, _ := json.Marshal(cl)
	signed := base64.RawURLEncoding.EncodeToString(head) + "." + base64.RawURLEncoding.EncodeToString(body)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, kc.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatal(err)
	}
	return signed + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func adminTestApp(kc *fakeKeycloak) *app {
	internal := kc.srv.URL + "/realms/ai-stack"
	a := &app{
		cfg: config{
			cookieKey: []byte(strings.Repeat("c", 32)), issuer: testIssuer, internalIssuer: internal,
			clientID: "pat-dashboard", publicOrigin: "https://ai.example.com",
		},
		http: kc.srv.Client(),
	}
	a.directory = newDirectory(internal, "pat-directory", "secret", a.http)
	return a
}

func sessionCookie(t *testing.T, a *app, s session) *http.Cookie {
	t.Helper()
	rec := httptest.NewRecorder()
	if err := a.setSignedCookie(rec, cookieName, s, 3600); err != nil {
		t.Fatal(err)
	}
	return rec.Result().Cookies()[0]
}

func liveSession(sub string) session {
	return session{Issuer: testIssuer, Subject: sub, Username: sub, Roles: []string{"ai-user"}, Expires: time.Now().Add(time.Hour).Unix()}
}

// guarded returns a handler that answers 204 once adminGuard admits.
func guarded(a *app) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if _, _, ok := a.adminGuard(w, r); ok {
			w.WriteHeader(http.StatusNoContent)
		}
	}
}

func adminRequest(a *app, method string, cookie *http.Cookie, csrf bool) *http.Request {
	r := httptest.NewRequest(method, "/api/admin/test", nil)
	if cookie != nil {
		r.AddCookie(cookie)
		if csrf {
			r.Header.Set("X-CSRF-Token", a.csrfToken(r))
			r.Header.Set("Origin", "https://ai.example.com")
		}
	}
	return r
}

func errorCode(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return body.Error.Code
}

func TestAdminGuardAccess(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-user", "ai-admin", "default-roles-ai-stack"}})
	kc.setUser("user", fakeUser{username: "user", enabled: true, roles: []string{"ai-user"}})
	kc.setUser("disabled", fakeUser{username: "disabled", enabled: false, roles: []string{"ai-admin"}})

	foreign := liveSession("admin")
	foreign.Issuer = "https://ai.example.com/sso/realms/master"
	expired := liveSession("admin")
	expired.Expires = time.Now().Add(-time.Minute).Unix()
	// A cookie claiming ai-admin does not matter: Keycloak is asked.
	staleRoles := liveSession("user")
	staleRoles.Roles = []string{"ai-user", "ai-admin"}

	cases := []struct {
		name   string
		cookie *http.Cookie
		status int
		code   string
	}{
		{"anonymous", nil, 401, "unauthenticated"},
		{"expired", sessionCookie(t, a, expired), 401, "unauthenticated"},
		{"foreign realm", sessionCookie(t, a, foreign), 403, "forbidden"},
		{"ordinary user", sessionCookie(t, a, liveSession("user")), 403, "forbidden"},
		{"cookie roles ignored", sessionCookie(t, a, staleRoles), 403, "forbidden"},
		{"disabled admin", sessionCookie(t, a, liveSession("disabled")), 403, "forbidden"},
		{"deleted admin", sessionCookie(t, a, liveSession("gone")), 403, "forbidden"},
		{"admin", sessionCookie(t, a, liveSession("admin")), 204, ""},
	}
	for _, tc := range cases {
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			rec := httptest.NewRecorder()
			guarded(a)(rec, adminRequest(a, method, tc.cookie, true))
			if rec.Code != tc.status || errorCode(t, rec) != tc.code {
				t.Errorf("%s %s: got %d %q, want %d %q", tc.name, method, rec.Code, errorCode(t, rec), tc.status, tc.code)
			}
		}
	}
}

func TestAdminGuardRejectsPAT(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	r := httptest.NewRequest(http.MethodGet, "/api/admin/users", nil)
	r.Header.Set("Authorization", "Bearer sk-admin-owned-token")
	rec := httptest.NewRecorder()
	guarded(a)(rec, r)
	if rec.Code != 401 {
		t.Fatalf("PAT got %d, want 401", rec.Code)
	}
}

func TestAdminGuardCSRF(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	cookie := sessionCookie(t, a, liveSession("admin"))

	for name, mutate := range map[string]func(*http.Request){
		"missing token":    func(r *http.Request) { r.Header.Del("X-CSRF-Token") },
		"wrong token":      func(r *http.Request) { r.Header.Set("X-CSRF-Token", "forged") },
		"foreign origin":   func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"cross-site fetch": func(r *http.Request) { r.Header.Del("Origin"); r.Header.Set("Sec-Fetch-Site", "cross-site") },
	} {
		r := adminRequest(a, http.MethodPost, cookie, true)
		mutate(r)
		rec := httptest.NewRecorder()
		guarded(a)(rec, r)
		if rec.Code != 403 || errorCode(t, rec) != "csrf_failed" {
			t.Errorf("%s: got %d %q, want 403 csrf_failed", name, rec.Code, errorCode(t, rec))
		}
	}
	// The token is bound to the session cookie it was issued for.
	other := sessionCookie(t, a, liveSession("admin2"))
	r := adminRequest(a, http.MethodPost, cookie, true)
	r.Header.Set("X-CSRF-Token", a.csrfToken(adminRequest(a, http.MethodGet, other, false)))
	rec := httptest.NewRecorder()
	guarded(a)(rec, r)
	if rec.Code != 403 {
		t.Errorf("token from another session: got %d, want 403", rec.Code)
	}
	// Reads need no CSRF token.
	rec = httptest.NewRecorder()
	guarded(a)(rec, adminRequest(a, http.MethodGet, cookie, false))
	if rec.Code != 204 {
		t.Errorf("GET without token: got %d, want 204", rec.Code)
	}
}

func TestUserMutationsRequireCSRF(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	cookie := sessionCookie(t, a, liveSession("user"))
	for _, path := range []string{"/api/tokens", "/api/tokens/abc", "/api/agent-token"} {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"name":"x"}`))
		r.AddCookie(cookie)
		rec := httptest.NewRecorder()
		newMux(a).ServeHTTP(rec, r)
		if rec.Code != 403 {
			t.Errorf("POST %s without CSRF token: got %d, want 403", path, rec.Code)
		}
	}
}

func TestAdminMembershipCacheAndFreshness(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	cookie := sessionCookie(t, a, liveSession("admin"))
	serve := func(method string) int {
		rec := httptest.NewRecorder()
		guarded(a)(rec, adminRequest(a, method, cookie, true))
		return rec.Code
	}
	if serve(http.MethodGet) != 204 {
		t.Fatal("admin read refused")
	}
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-user"}})
	// Reads may trust the check for up to adminMembershipTTL...
	if got := serve(http.MethodGet); got != 204 {
		t.Fatalf("cached read: got %d, want 204", got)
	}
	// ...but a mutation re-checks and sees the removal at once.
	if got := serve(http.MethodPost); got != 403 {
		t.Fatalf("mutation after role removal: got %d, want 403", got)
	}
	// The fresh result replaced the cache, so reads stop too.
	if got := serve(http.MethodGet); got != 403 {
		t.Fatalf("read after fresh check: got %d, want 403", got)
	}
	a.directory.members["admin"] = adminMembership{admin: true, checked: time.Now().Add(-2 * adminMembershipTTL)}
	if got := serve(http.MethodGet); got != 403 {
		t.Fatalf("expired cache entry: got %d, want 403", got)
	}
}

func TestAdminGuardDeniesWhenDirectoryUnavailable(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	cookie := sessionCookie(t, a, liveSession("admin"))
	rec := httptest.NewRecorder()
	guarded(a)(rec, adminRequest(a, http.MethodGet, cookie, false))
	if rec.Code != 204 {
		t.Fatalf("warm-up read: got %d", rec.Code)
	}
	kc.down.Store(true)
	rec = httptest.NewRecorder()
	guarded(a)(rec, adminRequest(a, http.MethodPost, cookie, true))
	if rec.Code != 503 || errorCode(t, rec) != "directory_unavailable" {
		t.Fatalf("mutation with Keycloak down: got %d %q, want 503", rec.Code, errorCode(t, rec))
	}

	a.directory = nil
	rec = httptest.NewRecorder()
	guarded(a)(rec, adminRequest(a, http.MethodGet, cookie, false))
	if rec.Code != 503 {
		t.Fatalf("no directory configured: got %d, want 503", rec.Code)
	}
}

func TestSessionInfoCapabilities(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	kc.setUser("user", fakeUser{username: "user", enabled: true, roles: []string{"ai-user"}})
	for sub, want := range map[string]string{"admin": `["admin"]`, "user": `[]`} {
		r := httptest.NewRequest(http.MethodGet, "/api/session", nil)
		r.AddCookie(sessionCookie(t, a, liveSession(sub)))
		rec := httptest.NewRecorder()
		newMux(a).ServeHTTP(rec, r)
		var body struct {
			Capabilities json.RawMessage `json:"capabilities"`
		}
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil || string(body.Capabilities) != want {
			t.Errorf("%s: got %d %s, want capabilities %s", sub, rec.Code, rec.Body, want)
		}
	}
}

func TestVerifyJWTChecksAuthorizedParty(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	claims := func(mod func(map[string]any)) string {
		cl := map[string]any{"iss": testIssuer, "sub": "u1", "azp": "pat-dashboard", "exp": time.Now().Add(time.Hour).Unix()}
		mod(cl)
		return kc.sign(t, cl)
	}
	ctx := context.Background()
	if _, err := a.verifyJWT(ctx, claims(func(map[string]any) {}), "pat-dashboard"); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	for name, raw := range map[string]string{
		"other client":  claims(func(cl map[string]any) { cl["azp"] = "open-webui" }),
		"other realm":   claims(func(cl map[string]any) { cl["iss"] = "https://ai.example.com/sso/realms/master" }),
		"expired":       claims(func(cl map[string]any) { cl["exp"] = time.Now().Add(-time.Minute).Unix() }),
		"no subject":    claims(func(cl map[string]any) { delete(cl, "sub") }),
		"bad signature": func() string { raw := claims(func(map[string]any) {}); return raw[:len(raw)-8] + "AAAAAAAA" }(),
	} {
		if _, err := a.verifyJWT(ctx, raw, "pat-dashboard"); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// MCP accepts any client of the realm, as before.
	if _, err := a.verifyJWT(ctx, claims(func(cl map[string]any) { cl["azp"] = "open-webui" }), ""); err != nil {
		t.Errorf("unrestricted azp rejected: %v", err)
	}
}

func TestPublicOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://ai.example.com/platform/auth/callback": "https://ai.example.com",
		"http://pat.localhost:8080/auth/callback":       "http://pat.localhost:8080",
		"/auth/callback": "",
	} {
		if got := publicOrigin(in); got != want {
			t.Errorf("publicOrigin(%q) = %q, want %q", in, got, want)
		}
	}
}

// Logout must end the Keycloak SSO session too, or the next /auth/login
// signs the browser straight back in.
func TestLogoutEndsKeycloakSession(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	a.cfg.urlPrefix = "/platform"
	r := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	r.Header.Set("Origin", "https://ai.example.com")
	r.AddCookie(sessionCookie(t, a, liveSession("alice")))
	r.AddCookie(&http.Cookie{Name: idTokenCookieName, Value: "id.token.value"})
	rec := httptest.NewRecorder()
	newMux(a).ServeHTTP(rec, r)
	var body map[string]string
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	u, err := url.Parse(body["logout_url"])
	if err != nil || u.Scheme+"://"+u.Host+u.Path != testIssuer+"/protocol/openid-connect/logout" {
		t.Fatalf("logout_url = %q", body["logout_url"])
	}
	q := u.Query()
	if q.Get("id_token_hint") != "id.token.value" || q.Get("client_id") != "pat-dashboard" ||
		q.Get("post_logout_redirect_uri") != "https://ai.example.com/platform/" {
		t.Errorf("query = %v", q)
	}
	cleared := map[string]bool{}
	for _, c := range rec.Result().Cookies() {
		cleared[c.Name] = c.MaxAge < 0
	}
	if !cleared[cookieName] || !cleared[idTokenCookieName] {
		t.Errorf("cookies not cleared: %v", cleared)
	}
	cross := httptest.NewRequest(http.MethodPost, "/auth/logout", nil)
	cross.Header.Set("Origin", "https://evil.example")
	rec = httptest.NewRecorder()
	newMux(a).ServeHTTP(rec, cross)
	if rec.Code != 403 {
		t.Errorf("cross-origin logout: %d", rec.Code)
	}
}

// --- Postgres-backed audit tests (PATDB_TEST_DSN) ---

func migratedAdminApp(t *testing.T) (*app, *fakeKeycloak) {
	t.Helper()
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	a.db = testDB(t)
	if err := a.migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	return a, kc
}

func auditRows(t *testing.T, a *app) []auditRecord {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/api/admin/audit?limit=200", nil)
	r.AddCookie(sessionCookie(t, a, liveSession("admin")))
	rec := httptest.NewRecorder()
	newMux(a).ServeHTTP(rec, r)
	if rec.Code != 200 {
		t.Fatalf("audit list: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Events []auditRecord `json:"events"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	return body.Events
}

func TestAuditIsAppendOnly(t *testing.T) {
	a, _ := migratedAdminApp(t)
	ctx := context.Background()
	if err := insertAudit(ctx, a.db, auditEvent{Action: "test", Source: "api", Outcome: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`UPDATE admin_audit_events SET outcome='failed'`,
		`DELETE FROM admin_audit_events`,
		`TRUNCATE admin_audit_events`,
	} {
		if _, err := a.db.Exec(ctx, stmt); err == nil {
			t.Errorf("%s succeeded", stmt)
		}
	}
}

func TestDeniedMutationsAreAudited(t *testing.T) {
	a, kc := migratedAdminApp(t)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	kc.setUser("user", fakeUser{username: "user", enabled: true, roles: []string{"ai-user"}})

	forged := adminRequest(a, http.MethodPost, nil, false)
	forged.Header.Set("X-User-Id", "admin") // never trusted for attribution
	guarded(a)(httptest.NewRecorder(), forged)
	guarded(a)(httptest.NewRecorder(), adminRequest(a, http.MethodPost, sessionCookie(t, a, liveSession("user")), true))
	guarded(a)(httptest.NewRecorder(), adminRequest(a, http.MethodPost, sessionCookie(t, a, liveSession("admin")), false))
	// Denied reads are not mutation attempts.
	guarded(a)(httptest.NewRecorder(), adminRequest(a, http.MethodGet, nil, false))

	events := auditRows(t, a)
	want := []struct{ subject, code string }{{"admin", "csrf_failed"}, {"user", "forbidden"}, {"", "unauthenticated"}}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		e := events[i]
		if e.Outcome != "denied" || e.Actor.Subject != w.subject || e.ErrorCode != w.code || e.RequestID == "" {
			t.Errorf("event %d = %+v, want denied %s/%s", i, e, w.subject, w.code)
		}
	}
}

// mutateHandler is a stand-in for a future admin mutation endpoint: it
// writes a row to a scratch table through adminMutate.
func mutateHandler(a *app, fail error, after any) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, requestID, ok := a.adminGuard(w, r)
		if !ok {
			return
		}
		a.adminMutate(w, r, actor, requestID, auditEvent{Action: "scratch.update", TargetType: "scratch", TargetID: "1"},
			func(ctx context.Context, tx pgx.Tx) (any, any, error) {
				if _, err := tx.Exec(ctx, `INSERT INTO scratch VALUES (1)`); err != nil {
					return nil, nil, err
				}
				return map[string]int{"value": 0}, after, fail
			})
	}
}

func TestAdminMutateCommitsChangeWithAudit(t *testing.T) {
	a, kc := migratedAdminApp(t)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	ctx := context.Background()
	if _, err := a.db.Exec(ctx, `CREATE TABLE scratch (v int)`); err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie(t, a, liveSession("admin"))
	scratchRows := func() (n int) {
		_ = a.db.QueryRow(ctx, `SELECT count(*) FROM scratch`).Scan(&n)
		return n
	}

	r := adminRequest(a, http.MethodPost, cookie, true)
	r.Header.Set("X-Admin-Source", "ui")
	rec := httptest.NewRecorder()
	mutateHandler(a, nil, map[string]int{"value": 1})(rec, r)
	if rec.Code != 200 || scratchRows() != 1 {
		t.Fatalf("success: got %d, rows %d", rec.Code, scratchRows())
	}

	rec = httptest.NewRecorder()
	mutateHandler(a, &apiErr{409, "revision_conflict", "stale edit"}, nil)(rec, adminRequest(a, http.MethodPost, cookie, true))
	if rec.Code != 409 || scratchRows() != 1 {
		t.Fatalf("conflict: got %d, rows %d", rec.Code, scratchRows())
	}

	rec = httptest.NewRecorder()
	mutateHandler(a, errors.New("boom"), nil)(rec, adminRequest(a, http.MethodPost, cookie, true))
	if rec.Code != 500 || scratchRows() != 1 {
		t.Fatalf("failure: got %d, rows %d", rec.Code, scratchRows())
	}

	// An audit insert that fails must undo the change: a channel cannot be
	// marshalled into after_value.
	rec = httptest.NewRecorder()
	mutateHandler(a, nil, make(chan int))(rec, adminRequest(a, http.MethodPost, cookie, true))
	if rec.Code != 503 || scratchRows() != 1 {
		t.Fatalf("audit failure: got %d, rows %d", rec.Code, scratchRows())
	}

	events := auditRows(t, a)
	want := []struct{ outcome, code string }{{"failed", "internal"}, {"denied", "revision_conflict"}, {"succeeded", ""}}
	if len(events) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(events), len(want), events)
	}
	for i, w := range want {
		if events[i].Outcome != w.outcome || events[i].ErrorCode != w.code || events[i].Actor.Subject != "admin" {
			t.Errorf("event %d = %+v, want %s/%s", i, events[i], w.outcome, w.code)
		}
	}
	ok := events[2]
	if ok.Source != "ui" || string(ok.Before) != `{"value":0}` || string(ok.After) != `{"value":1}` || ok.TargetType != "scratch" {
		t.Errorf("succeeded event = %+v", ok)
	}
}

func TestAdminAuditFiltersAndPages(t *testing.T) {
	a, kc := migratedAdminApp(t)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin"}})
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		outcome := "succeeded"
		if i%2 == 1 {
			outcome = "denied"
		}
		if err := insertAudit(ctx, a.db, auditEvent{Action: "quota.update", Actor: adminActor{Subject: "admin"}, Source: "api", Outcome: outcome, OperationID: "op-" + string(rune('a'+i))}); err != nil {
			t.Fatal(err)
		}
	}
	get := func(query string) (int, map[string]any) {
		r := httptest.NewRequest(http.MethodGet, "/api/admin/audit?"+query, nil)
		r.AddCookie(sessionCookie(t, a, liveSession("admin")))
		rec := httptest.NewRecorder()
		newMux(a).ServeHTTP(rec, r)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	count := func(body map[string]any) int { return len(body["events"].([]any)) }

	if _, body := get("outcome=denied"); count(body) != 2 {
		t.Errorf("outcome filter: %d events", count(body))
	}
	if _, body := get("operation_id=op-c"); count(body) != 1 {
		t.Errorf("operation filter: %d events", count(body))
	}
	_, first := get("limit=3")
	next := first["next_before_id"].(string)
	if count(first) != 3 || next == "" {
		t.Fatalf("first page: %d events, next %q", count(first), next)
	}
	if _, second := get("limit=3&before_id=" + next); count(second) != 2 || second["next_before_id"] != "" {
		t.Errorf("second page: %d events, next %v", count(second), second["next_before_id"])
	}
	if code, _ := get("from=2020-01-01T00:00:00Z&to=2026-01-01T00:00:00Z"); code != 400 {
		t.Errorf("over-long range: got %d, want 400", code)
	}
}

func TestAdminUsersJoinsDirectoryAndUsage(t *testing.T) {
	a, kc := migratedAdminApp(t)
	kc.setUser("admin", fakeUser{username: "admin", enabled: true, roles: []string{"ai-admin", "offline_access"}})
	kc.setUser("idle", fakeUser{username: "idle", enabled: false, roles: []string{"ai-user"}})
	ctx := context.Background()
	if _, err := a.db.Exec(ctx, `INSERT INTO qos_events (owner_issuer, owner_subject, model, band, prompt_tokens, completion_tokens, cost_amount, created_at) VALUES
		($1, 'admin', 'm', 'normal', 100, 20, 1.5, now()),
		($1, 'admin', 'm', 'normal', 1000, 0, 9, now() - interval '40 days'),
		('https://other/realms/x', 'admin', 'm', 'normal', 5, 5, 0, now())`, testIssuer); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/admin/users?days=30", nil)
	r.AddCookie(sessionCookie(t, a, liveSession("admin")))
	rec := httptest.NewRecorder()
	newMux(a).ServeHTTP(rec, r)
	var body struct {
		Users    []adminUserRow `json:"users"`
		Total    int            `json:"total"`
		Coverage string         `json:"coverage"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &body) != nil {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	if body.Total != 2 || len(body.Users) != 2 || body.Coverage != "pat_only" {
		t.Fatalf("body = %+v", body)
	}
	for _, u := range body.Users {
		switch u.Subject {
		case "admin":
			if u.Tokens != 120 || u.Requests != 1 || u.LastActivityAt == nil || len(u.Roles) != 1 || u.Roles[0] != "ai-admin" {
				t.Errorf("admin row = %+v", u)
			}
		case "idle":
			if u.Tokens != 0 || u.LastActivityAt != nil || u.Enabled {
				t.Errorf("idle row = %+v", u)
			}
		}
	}
}

func TestDashboardInjectsSessionCSRFToken(t *testing.T) {
	kc := newFakeKeycloak(t)
	a := adminTestApp(kc)
	r := httptest.NewRequest(http.MethodGet, "/admin/audit", nil)
	r.AddCookie(sessionCookie(t, a, liveSession("user")))
	rec := httptest.NewRecorder()
	newMux(a).ServeHTTP(rec, r)
	want := `<meta name="pat-csrf" content="` + a.csrfToken(r) + `"`
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), want) {
		t.Fatalf("got %d, body lacks %s", rec.Code, want)
	}
}
