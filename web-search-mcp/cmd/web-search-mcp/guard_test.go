package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

type fakeResolver map[string][]string

func (f fakeResolver) LookupNetIP(_ context.Context, _, host string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, s := range f[host] {
		out = append(out, netip.MustParseAddr(s))
	}
	if out == nil {
		return nil, errors.New("no such host")
	}
	return out, nil
}

func mustGuard(t *testing.T, extra string) *guard {
	t.Helper()
	g, err := newGuard(extra)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestAllowedIP(t *testing.T) {
	g := mustGuard(t, "")
	for _, s := range []string{
		"10.43.0.10", "172.20.1.1", "192.168.1.1", "127.0.0.1", "169.254.169.254", "100.64.0.1",
		"0.0.0.0", "::1", "::", "fd00::1", "fe80::1", "::ffff:10.0.0.1", "::ffff:127.0.0.1", "64:ff9b::a00:1",
	} {
		if g.allowedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "93.184.216.34", "2606:4700::1111"} {
		if !g.allowedIP(netip.MustParseAddr(s)) {
			t.Errorf("%s denied", s)
		}
	}
}

func TestExtraDenyCIDRs(t *testing.T) {
	g := mustGuard(t, "203.0.113.0/24, 2001:db8::/32")
	if g.allowedIP(netip.MustParseAddr("203.0.113.5")) || g.allowedIP(netip.MustParseAddr("2001:db8::1")) {
		t.Fatal("extra CIDR not denied")
	}
	if _, err := newGuard("not-a-cidr"); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
}

func TestCheckURL(t *testing.T) {
	g := mustGuard(t, "")
	for _, raw := range []string{"file:///etc/passwd", "gopher://x/", "http://example.com:8080/", "https://u:p@example.com/", "http:///x"} {
		u, _ := url.Parse(raw)
		if err := g.checkURL(u); err == nil {
			t.Errorf("%s accepted", raw)
		}
	}
	for _, raw := range []string{"http://example.com/", "https://example.com:443/x"} {
		u, _ := url.Parse(raw)
		if err := g.checkURL(u); err != nil {
			t.Errorf("%s refused: %v", raw, err)
		}
	}
}

func TestDialRefusesPrivateAndMixedAnswers(t *testing.T) {
	g := mustGuard(t, "")
	g.resolver = fakeResolver{
		"private.test": {"10.0.0.5"},
		"mixed.test":   {"93.184.216.34", "10.0.0.5"},
		"v6.test":      {"::1"},
	}
	dial := g.dialContext(&net.Dialer{Timeout: time.Second})
	for _, addr := range []string{"private.test:443", "mixed.test:443", "v6.test:80", "169.254.169.254:80", "[::1]:443", "93.184.216.34:22"} {
		if _, err := dial(context.Background(), "tcp", addr); !errors.Is(err, errBlocked) {
			t.Errorf("%s: want errBlocked, got %v", addr, err)
		}
	}
}

func testApp(t *testing.T, g *guard) *app {
	t.Helper()
	return &app{cfg: config{fetchMaxChars: 1000, fetchMaxBytes: 1 << 20}, guard: g, fetch: g.client(5 * time.Second)}
}

func TestFetchRefusesRedirectToPrivate(t *testing.T) {
	// The test server itself is on loopback, so this guard denies only the
	// redirect target, which resolves to a private address.
	g := &guard{anyPort: true, resolver: fakeResolver{"internal.test": {"10.0.0.5"}}}
	for _, s := range []string{"10.0.0.0/8", "169.254.0.0/16"} {
		g.deny = append(g.deny, netip.MustParsePrefix(s))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/to-private":
			http.Redirect(w, r, "http://internal.test/secret", http.StatusFound)
		case "/to-metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
		case "/to-file":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		}
	}))
	defer srv.Close()
	a := testApp(t, g)
	for _, p := range []string{"/to-private", "/to-metadata", "/to-file"} {
		if _, err := a.fetchText(context.Background(), srv.URL+p, 100); err == nil {
			t.Errorf("%s: redirect followed", p)
		}
	}
}

func TestFetchRedirectLimit(t *testing.T) {
	g := &guard{anyPort: true, resolver: fakeResolver{}}
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+r.URL.Path+"x", http.StatusFound)
	}))
	defer srv.Close()
	if _, err := testApp(t, g).fetchText(context.Background(), srv.URL+"/", 100); err == nil || !strings.Contains(err.Error(), "too many redirects") {
		t.Fatalf("want redirect limit, got %v", err)
	}
}

func TestFetchHTMLToTextAndTruncate(t *testing.T) {
	g := &guard{anyPort: true, resolver: fakeResolver{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/page":
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Write([]byte(`<html><head><title>T</title><style>.x{}</style><script>evil()</script></head><body><p>Hello   world</p><div>second</div></body></html>`))
		case "/bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte{0, 1, 2})
		case "/long":
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(strings.Repeat("я", 50)))
		}
	}))
	defer srv.Close()
	a := testApp(t, g)
	text, err := a.fetchText(context.Background(), srv.URL+"/page", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "evil") || strings.Contains(text, ".x{}") || !strings.Contains(text, "Hello world\nsecond") {
		t.Fatalf("unexpected text %q", text)
	}
	if _, err := a.fetchText(context.Background(), srv.URL+"/bin", 100); err == nil {
		t.Fatal("binary content accepted")
	}
	text, err = a.fetchText(context.Background(), srv.URL+"/long", 10)
	if err != nil || !strings.Contains(text, strings.Repeat("я", 10)+"\n[truncated]") || strings.Contains(text, strings.Repeat("я", 11)) {
		t.Fatalf("truncation: %q %v", text, err)
	}
}

func TestWebSearchCallsPinnedSidecarPath(t *testing.T) {
	var got url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/mega/search" {
			http.NotFound(w, r)
			return
		}
		got = r.URL.Query()
		w.Write([]byte(`{"results":[{"url":"https://a","title":"A","snippet":"sa"},{"url":"https://b","title":"B","snippet":"sb"},{"url":"https://c","title":"C","snippet":"sc"}]}`))
	}))
	defer srv.Close()
	a := &app{cfg: config{searchURL: srv.URL}, search: srv.Client()}
	res, _, err := a.webSearch(context.Background(), nil, searchIn{Query: " go 1.26 ", Limit: 2})
	if err != nil || res.IsError {
		t.Fatalf("%v %+v", err, res)
	}
	if got.Get("text") != "go 1.26" || got.Get("limit") != "2" {
		t.Fatalf("query %v", got)
	}
	out := res.Content[0].(interface{ MarshalJSON() ([]byte, error) })
	b, _ := out.MarshalJSON()
	if !strings.Contains(string(b), "UNTRUSTED WEB CONTENT") || !strings.Contains(string(b), "https://b") || strings.Contains(string(b), "https://c") {
		t.Fatalf("result %s", b)
	}
	res, _, _ = a.webSearch(context.Background(), nil, searchIn{Query: "q", Limit: 50})
	if got.Get("limit") != "10" || res.IsError {
		t.Fatalf("limit not capped: %v", got)
	}
}
