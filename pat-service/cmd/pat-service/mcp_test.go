package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeWindow struct{ n map[string]int64 }

func (f *fakeWindow) IncrWindow(_ context.Context, key string, _ time.Duration) (int64, error) {
	f.n[key]++
	return f.n[key], nil
}

type failingWindow struct{}

func (failingWindow) IncrWindow(context.Context, string, time.Duration) (int64, error) {
	return 0, errors.New("valkey down")
}

func mcpTestApp(upstream string) *app {
	a := testApp()
	a.cfg.mcpServers = map[string]string{"web-search": upstream}
	a.cfg.mcpCallsPerMinute = 2
	a.mcpLimiter = &fakeWindow{n: map[string]int64{}}
	return a
}

func TestMuxRegistersMCPRoute(t *testing.T) {
	mux := newMux(&app{})
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete} {
		_, pattern := mux.Handler(httptest.NewRequest(method, "/mcp/web-search/", nil))
		if pattern != method+" /mcp/" {
			t.Fatalf("%s matched %q", method, pattern)
		}
	}
}

func TestMCPDisabledIs404(t *testing.T) {
	a := testApp()
	rec := httptest.NewRecorder()
	a.mcpProxy(rec, httptest.NewRequest(http.MethodPost, "/mcp/web-search/", strings.NewReader("{}")))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status %d", rec.Code)
	}
	a = mcpTestApp("http://127.0.0.1:1")
	for _, path := range []string{"/mcp/repowise/", "/mcp/web-search", "/mcp/", "/mcp//"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer sk-abc")
		a.mcpProxy(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status %d", path, rec.Code)
		}
	}
}

func TestMCPRejectsMissingOrMalformedCredentials(t *testing.T) {
	a := mcpTestApp("http://127.0.0.1:1")
	for _, auth := range []string{"", "sk-abc", "Bearer ", "Bearer  sk-x", "Bearer not.a.jwt", "Bearer garbage"} {
		req := httptest.NewRequest(http.MethodPost, "/mcp/web-search/", strings.NewReader("{}"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		a.mcpProxy(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("%q: status %d", auth, rec.Code)
		}
	}
}

func TestForwardMCPReplacesIdentityAndStreams(t *testing.T) {
	var got *http.Request
	var body string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b, _ := io.ReadAll(r.Body)
		body = string(b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("event: message\ndata: {}\n\n"))
	}))
	defer up.Close()
	a := mcpTestApp(up.URL)

	payload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"web_search","arguments":{"query":"x"}}}`
	req := httptest.NewRequest(http.MethodPost, "/mcp/web-search/?x=1", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("X-User-Id", "spoofed")
	req.Header.Set("X-User-Name", "spoofed")
	req.Header.Set("X-Pat-Token-Id", "spoofed")
	req.Header.Set("Cookie", "pat_session=abc")
	req.Header.Set("Mcp-Session-Id", "s1")
	rec := httptest.NewRecorder()
	a.forwardMCP(rec, req, mcpServer{"web-search", up.URL}, mcpCaller{subject: "sub-1", name: "alice", tokenID: "tok-1"})

	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "data: {}") {
		t.Fatalf("response %d %q", rec.Code, rec.Body.String())
	}
	if got.URL.Path != "/" || got.URL.RawQuery != "x=1" {
		t.Fatalf("upstream path %q query %q", got.URL.Path, got.URL.RawQuery)
	}
	if got.Header.Get("X-User-Id") != "sub-1" || got.Header.Get("X-User-Name") != "alice" || got.Header.Get("X-Pat-Token-Id") != "tok-1" {
		t.Fatalf("identity headers %v", got.Header)
	}
	if got.Header.Get("Authorization") != "" || got.Header.Get("Cookie") != "" {
		t.Fatal("client credential forwarded")
	}
	if got.Header.Get("Mcp-Session-Id") != "s1" || body != payload {
		t.Fatalf("protocol header or body changed: %q", body)
	}
	if v := testutil.ToFloat64(a.qos.Metrics().MCPCallsTotal.WithLabelValues("sub-1", "web-search", "web_search", "200")); v != 1 {
		t.Fatalf("metric %v", v)
	}
}

func TestForwardMCPJWTCallerHasNoTokenID(t *testing.T) {
	var got http.Header
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { got = r.Header }))
	defer up.Close()
	a := mcpTestApp(up.URL)
	req := httptest.NewRequest(http.MethodPost, "/mcp/web-search/", strings.NewReader(`{"method":"initialize"}`))
	req.Header.Set("X-Pat-Token-Id", "spoofed")
	a.forwardMCP(httptest.NewRecorder(), req, mcpServer{"web-search", up.URL}, mcpCaller{subject: "sub-2"})
	if got.Get("X-Pat-Token-Id") != "" || got.Get("X-User-Name") != "sub-2" {
		t.Fatalf("headers %v", got)
	}
}

func TestForwardMCPRateLimitsToolCallsOnly(t *testing.T) {
	calls := 0
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer up.Close()
	a := mcpTestApp(up.URL)
	send := func(body string) int {
		rec := httptest.NewRecorder()
		a.forwardMCP(rec, httptest.NewRequest(http.MethodPost, "/mcp/web-search/", strings.NewReader(body)), mcpServer{"web-search", up.URL}, mcpCaller{subject: "u"})
		return rec.Code
	}
	call := `{"method":"tools/call","params":{"name":"fetch_url"}}`
	if send(call) != 200 || send(call) != 200 {
		t.Fatal("calls under the limit refused")
	}
	if send(call) != http.StatusTooManyRequests {
		t.Fatal("third call not limited")
	}
	if send(`{"method":"tools/list"}`) != 200 {
		t.Fatal("tools/list limited")
	}
	if calls != 3 {
		t.Fatalf("upstream saw %d requests", calls)
	}
	if v := testutil.ToFloat64(a.qos.Metrics().MCPCallsTotal.WithLabelValues("u", "web-search", "fetch_url", "429")); v != 1 {
		t.Fatalf("429 metric %v", v)
	}

	a.mcpLimiter = failingWindow{}
	if send(call) != 200 {
		t.Fatal("limiter failure must fail open")
	}
}

func TestForwardMCPPathMountedUpstream(t *testing.T) {
	var paths []string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { paths = append(paths, r.URL.Path) }))
	defer up.Close()
	a := mcpTestApp(up.URL)
	srv := mcpServer{"repowise", up.URL + "/mcp"}
	for _, p := range []string{"/mcp/repowise/", "/mcp/repowise/x"} {
		a.forwardMCP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, p, strings.NewReader(`{"method":"initialize"}`)), srv, mcpCaller{subject: "u"})
	}
	if strings.Join(paths, ",") != "/mcp,/mcp/x" {
		t.Fatalf("upstream paths %v", paths)
	}
}

func TestMCPRateLimitIsPerServer(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer up.Close()
	a := mcpTestApp(up.URL)
	call := `{"method":"tools/call","params":{"name":"x"}}`
	send := func(name string) int {
		rec := httptest.NewRecorder()
		a.forwardMCP(rec, httptest.NewRequest(http.MethodPost, "/mcp/"+name+"/", strings.NewReader(call)), mcpServer{name, up.URL}, mcpCaller{subject: "u"})
		return rec.Code
	}
	send("web-search")
	send("web-search")
	if send("web-search") != http.StatusTooManyRequests {
		t.Fatal("web-search not limited")
	}
	if send("repowise") != 200 {
		t.Fatal("repowise limited by web-search calls")
	}
}

func TestForwardMCPUpstreamDownIs502(t *testing.T) {
	a := mcpTestApp("http://127.0.0.1:1")
	rec := httptest.NewRecorder()
	a.forwardMCP(rec, httptest.NewRequest(http.MethodPost, "/mcp/web-search/", strings.NewReader(`{}`)), mcpServer{"web-search", "http://127.0.0.1:1"}, mcpCaller{subject: "u"})
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status %d", rec.Code)
	}
}

func TestMCPToolName(t *testing.T) {
	for body, want := range map[string]string{
		`{"method":"tools/call","params":{"name":"web_search"}}`: "web_search",
		`{"method":"tools/call","params":{"name":"fetch_url"}}`:  "fetch_url",
		`{"method":"tools/call","params":{"name":"x"}}`:          "other",
		`{"method":"initialize"}`:                                 "none",
		`[{"method":"tools/call"}]`:                               "other",
		strings.Repeat(" ", mcpMaxInspectBody+1):                  "other",
	} {
		req := httptest.NewRequest(http.MethodPost, "/mcp/web-search/", strings.NewReader(body))
		rc, got := mcpToolName(req)
		restored, _ := io.ReadAll(rc)
		if got != want || string(restored) != body {
			t.Fatalf("%.40q: got %q, body restored %v", body, got, string(restored) == body)
		}
	}
	if _, got := mcpToolName(httptest.NewRequest(http.MethodGet, "/mcp/web-search/", nil)); got != "none" {
		t.Fatalf("GET: %q", got)
	}
}
