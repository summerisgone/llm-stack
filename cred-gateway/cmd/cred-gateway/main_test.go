package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeStore map[string]string

func (f fakeStore) Token(_ context.Context, id, connector string) (string, error) {
	if t, ok := f[id+"/"+connector]; ok {
		return t, nil
	}
	return "", errNoConnection
}

const testToken = "github_pat_0123456789abcdefghijklmnop"

type seen struct {
	path, query, auth, cookie string
	body                      string
}

func setup(t *testing.T, upstreamStatus int, upstreamBody string) (*gateway, *seen) {
	t.Helper()
	got := &seen{}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.query, got.auth, got.cookie = r.URL.Path, r.URL.RawQuery, r.Header.Get("Authorization"), r.Header.Get("Cookie")
		b, _ := io.ReadAll(r.Body)
		got.body = string(b)
		w.Header().Set("Set-Cookie", "session=x")
		w.Header().Set("WWW-Authenticate", "Basic")
		w.WriteHeader(upstreamStatus)
		io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(up.Close)
	g := &gateway{
		store:        fakeStore{userID("alice") + "/github": testToken},
		githubURL:    up.URL,
		githubAPIURL: up.URL + "/api",
		denyRefs:     map[string]bool{"refs/heads/main": true},
		connectURL:   "https://ai.example.com/platform",
		proxy:        http.DefaultTransport,
	}
	return g, got
}

func do(g *gateway, method, target, user, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if user != "" {
		req.Header.Set("X-User-Id", user)
	}
	req.Header.Set("Authorization", "Bearer sk-agent-pat")
	req.Header.Set("Cookie", "a=b")
	rec := httptest.NewRecorder()
	g.ServeHTTP(rec, req)
	return rec
}

func TestGitInfoRefsInjectsToken(t *testing.T) {
	g, got := setup(t, 200, "ok")
	rec := do(g, "GET", "/cred/github/o/r.git/info/refs?service=git-upload-pack", "alice", "")
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	if got.path != "/o/r.git/info/refs" || got.query != "service=git-upload-pack" {
		t.Fatalf("upstream got %s?%s", got.path, got.query)
	}
	if !strings.HasPrefix(got.auth, "Basic ") || got.cookie != "" {
		t.Fatalf("auth %q cookie %q", got.auth, got.cookie)
	}
	if rec.Header().Get("Set-Cookie") != "" || rec.Header().Get("WWW-Authenticate") != "" {
		t.Fatal("upstream auth headers leaked")
	}
}

func TestRepoWithoutGitSuffix(t *testing.T) {
	g, got := setup(t, 200, "")
	do(g, "POST", "/cred/github/o/r/git-upload-pack", "alice", "0000")
	if got.path != "/o/r.git/git-upload-pack" {
		t.Fatalf("upstream path %s", got.path)
	}
}

func TestRejectsOutsideAllowlist(t *testing.T) {
	g, got := setup(t, 200, "")
	for _, c := range []struct{ method, path string }{
		{"GET", "/cred/github/o/r.git/info/refs?service=other"},
		{"GET", "/cred/github/o/r.git/git-upload-pack"},
		{"GET", "/cred/github/o/r/settings"},
		{"DELETE", "/cred/github/api/repos/o/r"},
		{"POST", "/cred/github/api/repos/o/r/hooks"},
		{"GET", "/cred/gitlab/o/r.git/info/refs?service=git-upload-pack"},
	} {
		if rec := do(g, c.method, c.path, "alice", ""); rec.Code != 404 {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	if got.path != "" {
		t.Fatalf("upstream reached: %s", got.path)
	}
}

func TestMissingIdentity(t *testing.T) {
	g, _ := setup(t, 200, "")
	if rec := do(g, "GET", "/cred/github/o/r.git/info/refs?service=git-upload-pack", "", ""); rec.Code != 401 {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestNoConnectionIsGitError(t *testing.T) {
	g, got := setup(t, 200, "")
	rec := do(g, "GET", "/cred/github/o/r.git/info/refs?service=git-upload-pack", "bob", "")
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "ERR no GitHub connection") {
		t.Fatalf("%d %q", rec.Code, rec.Body.String())
	}
	if got.path != "" {
		t.Fatal("upstream reached without a token")
	}
}

func TestAPIUsesBearer(t *testing.T) {
	g, got := setup(t, 201, "{}")
	rec := do(g, "POST", "/cred/github/api/repos/o/r/pulls", "alice", `{"head":"x","base":"main"}`)
	if rec.Code != 201 || got.path != "/api/repos/o/r/pulls" || got.auth != "Bearer "+testToken {
		t.Fatalf("%d %s %q", rec.Code, got.path, got.auth)
	}
}

func TestErrorBodyScrubbed(t *testing.T) {
	g, _ := setup(t, 401, "bad credentials "+testToken+" and ghp_abcdefghijklmnopqrstuvwxyz")
	rec := do(g, "GET", "/cred/github/api/user", "alice", "")
	if strings.Contains(rec.Body.String(), testToken) || strings.Contains(rec.Body.String(), "ghp_") {
		t.Fatalf("leaked: %q", rec.Body.String())
	}
}

func cmd(ref string) string {
	return pktLine("0000000000000000000000000000000000000000 1111111111111111111111111111111111111111 " + ref + "\x00report-status")
}

func TestPushToDeniedRef(t *testing.T) {
	g, got := setup(t, 200, "")
	rec := do(g, "POST", "/cred/github/o/r.git/git-receive-pack", "alice", cmd("refs/heads/main")+"0000PACK")
	if rec.Code != 403 || got.path != "" {
		t.Fatalf("%d upstream=%s", rec.Code, got.path)
	}
}

func TestPushToBranchReplaysBody(t *testing.T) {
	g, got := setup(t, 200, "")
	body := cmd("refs/heads/feature") + "0000PACKDATA"
	rec := do(g, "POST", "/cred/github/o/r.git/git-receive-pack", "alice", body)
	if rec.Code != 200 || got.body != body {
		t.Fatalf("%d body=%q", rec.Code, got.body)
	}
}

func TestMalformedPush(t *testing.T) {
	g, got := setup(t, 200, "")
	if rec := do(g, "POST", "/cred/github/o/r.git/git-receive-pack", "alice", "zzzz"); rec.Code != 403 || got.path != "" {
		t.Fatalf("%d", rec.Code)
	}
}
