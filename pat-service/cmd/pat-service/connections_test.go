package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeBao is enough of OpenBao's Kubernetes auth and KV v2 for the
// dashboard: it records what was written and refuses reads of secret data,
// like the connections-writer policy.
type fakeBao struct {
	mu     sync.Mutex
	stored map[string]string
}

func (f *fakeBao) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path == "/v1/auth/kubernetes/login" {
			io.WriteString(w, `{"auth":{"client_token":"t","lease_duration":3600}}`)
			return
		}
		if r.Header.Get("X-Vault-Token") != "t" {
			w.WriteHeader(403)
			return
		}
		key := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, "/v1/secret/data/"), "/v1/secret/metadata/")
		switch {
		case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/secret/data/"):
			var body struct {
				Data map[string]string `json:"data"`
			}
			json.NewDecoder(r.Body).Decode(&body)
			f.stored[key] = body.Data["token"]
			io.WriteString(w, `{}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/secret/metadata/"):
			if _, ok := f.stored[key]; !ok {
				w.WriteHeader(404)
				return
			}
			io.WriteString(w, `{"data":{"updated_time":"2026-10-14T10:00:00Z"}}`)
		case r.Method == "DELETE" && strings.HasPrefix(r.URL.Path, "/v1/secret/metadata/"):
			delete(f.stored, key)
			w.WriteHeader(204)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(403)
		}
	}
}

func connectionsTestApp(t *testing.T) (*app, *fakeBao) {
	t.Helper()
	fb := &fakeBao{stored: map[string]string{}}
	srv := httptest.NewServer(fb.handler(t))
	t.Cleanup(srv.Close)
	jwt := filepath.Join(t.TempDir(), "token")
	os.WriteFile(jwt, []byte("sa-jwt"), 0o600)
	a := &app{
		cfg: config{cookieKey: []byte(strings.Repeat("c", 32)), issuer: testIssuer, publicOrigin: "https://ai.example.com"},
		bao: &baoWriter{addr: srv.URL, mount: "secret", role: "pat-service", jwtFile: jwt, client: srv.Client()},
	}
	return a, fb
}

func connReq(t *testing.T, a *app, method, path, body string, csrf bool) *http.Request {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.AddCookie(sessionCookie(t, a, liveSession("alice")))
	if csrf {
		r.Header.Set("X-CSRF-Token", a.csrfToken(r))
		r.Header.Set("Origin", "https://ai.example.com")
	}
	return r
}

func TestConnectionsLifecycle(t *testing.T) {
	a, fb := connectionsTestApp(t)
	mux := newMux(a)
	token := "github_pat_" + strings.Repeat("A", 40)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, connReq(t, a, "PUT", "/api/connections/github", `{"token":"`+token+`"}`, true))
	if rec.Code != 204 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if fb.stored["users/"+connectionID("alice")+"/github"] != token {
		t.Fatalf("stored %v", fb.stored)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, connReq(t, a, "GET", "/api/connections", "", false))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"connected":true`) || strings.Contains(rec.Body.String(), token) {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, connReq(t, a, "DELETE", "/api/connections/github", "", true))
	if rec.Code != 204 || len(fb.stored) != 0 {
		t.Fatalf("delete: %d %v", rec.Code, fb.stored)
	}
}

func TestConnectionsRejects(t *testing.T) {
	a, fb := connectionsTestApp(t)
	mux := newMux(a)
	for _, c := range []struct {
		name, method, path, body string
		csrf                     bool
		want                     int
	}{
		{"no csrf", "PUT", "/api/connections/github", `{"token":"github_pat_` + strings.Repeat("A", 40) + `"}`, false, 403},
		{"not a token", "PUT", "/api/connections/github", `{"token":"hunter2"}`, true, 400},
		{"unknown connector", "PUT", "/api/connections/gitlab", `{"token":"x"}`, true, 404},
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, connReq(t, a, c.method, c.path, c.body, c.csrf))
		if rec.Code != c.want {
			t.Errorf("%s: %d", c.name, rec.Code)
		}
	}
	if len(fb.stored) != 0 {
		t.Fatalf("stored %v", fb.stored)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/api/connections", nil))
	if rec.Code != 401 {
		t.Fatalf("no session: %d", rec.Code)
	}
}

func TestCredProxyNeedsPAT(t *testing.T) {
	a := testApp()
	mux := newCredMux(a)
	for _, auth := range []string{"", "Bearer eyJhbGciOi.x.y", "Basic eDp5", "Bearer  sk-x"} {
		r := httptest.NewRequest("GET", "/cred/github/o/r.git/info/refs?service=git-upload-pack", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, r)
		if rec.Code != 401 {
			t.Errorf("%q: %d", auth, rec.Code)
		}
	}
}

func TestCredRouteNotOnPublicMux(t *testing.T) {
	_, pattern := newMux(&app{}).Handler(httptest.NewRequest("GET", "/cred/github/o/r.git/info/refs", nil))
	if strings.Contains(pattern, "/cred/") {
		t.Fatalf("public mux serves %q", pattern)
	}
}
