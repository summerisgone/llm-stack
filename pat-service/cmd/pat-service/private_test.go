package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatewayRecorder stands in for the private AI Gateway and keeps the
// headers of the last request it received.
type gatewayRecorder struct {
	mu   sync.Mutex
	last http.Header
}

func (g *gatewayRecorder) handler(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	g.last = r.Header.Clone()
	g.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"model":"qwen","choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":1}}`)
}

func (g *gatewayRecorder) header(name string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.last.Get(name)
}

func privateTestApp(t *testing.T, gw *gatewayRecorder) (*app, *fakeKeycloak) {
	t.Helper()
	kc := newFakeKeycloak(t)
	upstream := httptest.NewServer(http.HandlerFunc(gw.handler))
	t.Cleanup(upstream.Close)
	a := testApp()
	a.cfg.issuer, a.cfg.internalIssuer = testIssuer, kc.srv.URL+"/realms/ai-stack"
	a.cfg.gatewayURL, a.cfg.privateClients = upstream.URL, map[string]bool{"open-webui": true}
	a.http, a.proxyHTTP = kc.srv.Client(), upstream.Client()
	a.gateway = gatewayToken{value: "gateway-credential", expires: time.Now().Add(time.Hour)}
	return a, kc
}

func userToken(t *testing.T, kc *fakeKeycloak, mod func(map[string]any)) string {
	cl := map[string]any{"iss": testIssuer, "sub": "alice-sub", "azp": "open-webui", "preferred_username": "alice",
		"exp": time.Now().Add(time.Hour).Unix(), "realm_access": map[string]any{"roles": []string{"ai-user"}}}
	if mod != nil {
		mod(cl)
	}
	return kc.sign(t, cl)
}

const chatBody = `{"model":"qwen","messages":[{"role":"system","content":"s"},{"role":"user","content":"hi"}]}`

func privateCall(a *app, token string, forged map[string]string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	r.Header.Set("Authorization", "Bearer "+token)
	for k, v := range forged {
		r.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	newPrivateMux(a).ServeHTTP(rec, r)
	return rec
}

func TestPrivateEntryDerivesIdentityFromToken(t *testing.T) {
	gw := &gatewayRecorder{}
	a, kc := privateTestApp(t, gw)
	rec := privateCall(a, userToken(t, kc, nil), map[string]string{
		"X-User-Id": "mallory", "X-User-Name": "mallory", "X-Pat-Token-Id": "forged",
		"X-Llm-D-Inference-Fairness-Id": "mallory", "X-Llm-D-Inference-Objective": "warm", "X-Session-Key": "forged",
	})
	if rec.Code != 200 {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
	for header, want := range map[string]string{
		"X-User-Id": "alice-sub", "X-User-Name": "alice", "X-Llm-D-Inference-Fairness-Id": "alice-sub",
		"Authorization": "Bearer gateway-credential", "X-Pat-Token-Id": "",
	} {
		if got := gw.header(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	// Valkey is unreachable here, so the server-assigned band is normal, not
	// the forged warm.
	if got := gw.header("X-Llm-D-Inference-Objective"); got != "normal" {
		t.Errorf("objective = %q, want the server-assigned normal", got)
	}
	if gw.header("X-Session-Key") == "forged" {
		t.Error("client session key reached the gateway")
	}
}

func TestPrivateEntryRejects(t *testing.T) {
	gw := &gatewayRecorder{}
	a, kc := privateTestApp(t, gw)
	for name, tc := range map[string]struct {
		token  string
		status int
	}{
		"PAT":           {"sk-not-accepted-here", 401},
		"no token":      {"", 401},
		"bad signature": {userToken(t, kc, nil)[:40] + "x", 401},
		"expired":       {userToken(t, kc, func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), 401},
		"other realm":   {userToken(t, kc, func(c map[string]any) { c["iss"] = "https://ai.example.com/sso/realms/master" }), 401},
		"other client":  {userToken(t, kc, func(c map[string]any) { c["azp"] = "pat-gateway" }), 403},
		"no stack role": {userToken(t, kc, func(c map[string]any) { c["realm_access"] = map[string]any{"roles": []string{"offline_access"}} }), 403},
	} {
		if rec := privateCall(a, tc.token, nil); rec.Code != tc.status {
			t.Errorf("%s: got %d, want %d", name, rec.Code, tc.status)
		}
	}
	if gw.header("X-User-Id") != "" {
		t.Error("a rejected request reached the gateway")
	}
	// The private listener serves inference only.
	rec := httptest.NewRecorder()
	newPrivateMux(a).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tokens", nil))
	if rec.Code != 404 {
		t.Errorf("/api/tokens on the private listener: %d", rec.Code)
	}
}

// Fair-share parity: the same user and body through either entry point
// reach the gateway with the same fairness tenant and server-chosen band,
// so direct chat no longer bypasses EPP's per-user queues.
func TestPrivateEntryMatchesPATFairShareHeaders(t *testing.T) {
	gw := &gatewayRecorder{}
	a, kc := privateTestApp(t, gw)
	a.db = testDB(t)
	a.cfg.hashKey = []byte(strings.Repeat("h", 32))
	a.cfg.qosEventTimeout = time.Second
	ctx := context.Background()
	if err := a.migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.Exec(ctx, `INSERT INTO personal_access_tokens (id, owner_issuer, owner_subject, owner_name, token_hash, token_prefix, name, expires_at)
		VALUES ('tok', $1, 'alice-sub', 'alice', $2, 'sk-parity', 'cli', now() + interval '1 day')`, testIssuer, a.tokenHash("sk-parity-token")); err != nil {
		t.Fatal(err)
	}
	keys := []string{"X-User-Id", "X-User-Name", "X-Llm-D-Inference-Fairness-Id", "X-Llm-D-Inference-Objective"}
	snapshot := func() map[string]string {
		out := map[string]string{}
		for _, k := range keys {
			out[k] = gw.header(k)
		}
		return out
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody))
	r.Header.Set("Authorization", "Bearer sk-parity-token")
	a.proxy(httptest.NewRecorder(), r)
	viaPAT := snapshot()
	if rec := privateCall(a, userToken(t, kc, nil), nil); rec.Code != 200 {
		t.Fatalf("private: %d %s", rec.Code, rec.Body)
	}
	viaWebUI := snapshot()
	for _, k := range keys {
		if viaPAT[k] != viaWebUI[k] || viaPAT[k] == "" {
			t.Errorf("%s: PAT %q, private %q", k, viaPAT[k], viaWebUI[k])
		}
	}
	// One subject, one ledger: both requests are alice-sub's, told apart by
	// source only.
	rows, err := a.db.Query(ctx, `SELECT owner_issuer, owner_subject, source, prompt_tokens FROM qos_events ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var sources []string
	for rows.Next() {
		var issuer, subject, source string
		var prompt int64
		if err := rows.Scan(&issuer, &subject, &source, &prompt); err != nil {
			t.Fatal(err)
		}
		if issuer != testIssuer || subject != "alice-sub" || prompt != 5 {
			t.Errorf("row %s %s %d", issuer, subject, prompt)
		}
		sources = append(sources, source)
	}
	if strings.Join(sources, ",") != "pat,webui" {
		t.Errorf("sources = %v", sources)
	}
}

func TestCoverageMarker(t *testing.T) {
	a := &app{}
	if c := a.coverage(time.Now()); c["coverage"] != "pat_only" {
		t.Errorf("unset: %v", c)
	}
	since := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	a.cfg.coverageSince = &since
	if c := a.coverage(since.Add(time.Hour)); c["coverage"] != "all_paths" {
		t.Errorf("after cutover: %v", c)
	}
	if c := a.coverage(since.Add(-time.Hour)); c["coverage"] != "mixed" {
		t.Errorf("across cutover: %v", c)
	}
}
