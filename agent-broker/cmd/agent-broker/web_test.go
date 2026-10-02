package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
)

// webBackend runs every agent at the fake agent-adapter's address.
type webBackend struct {
	*fakeBackend
	endpoint string
}

func (b webBackend) Start(ctx context.Context, u User, ref AgentRef, spec StartSpec) (*Agent, error) {
	a, err := b.fakeBackend.Start(ctx, u, ref, spec)
	if err == nil {
		a.Endpoint = b.endpoint
	}
	return a, err
}

func TestWebProxyExchangesTokenAndRewritesHost(t *testing.T) {
	const authority = "dsh.example.com:8443"
	var seen *http.Request
	dsh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != authority {
			http.Error(w, "untrusted host", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("token") == "T" {
			http.SetCookie(w, &http.Cookie{Name: "dsh-auth-x", Value: "v"})
			w.Header().Set("Location", "./")
			w.WriteHeader(http.StatusSeeOther)
			return
		}
		if c, err := r.Cookie("dsh-auth-x"); err != nil || c.Value != "v" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		seen = r
		_, _ = w.Write([]byte("ui"))
	}))
	defer dsh.Close()
	adapter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/web-token" || r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "T"})
	}))
	defer adapter.Close()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	s := &server{auth: NewVerifier(testIssuer, jwksServer(t, key, "k1").URL, []string{"ai-user"})}
	s.backend = webBackend{newFake(), adapter.URL}
	s.slots = newTestSlots(s.backend, &clock{})
	dshURL, _ := url.Parse(dsh.URL)
	port, _ := strconv.Atoi(dshURL.Port())
	proxy := httptest.NewServer((&webProxy{s: s, authority: authority, port: port, client: http.DefaultClient}).routes())
	defer proxy.Close()
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	get := func(authz, cookie string) *http.Response {
		req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/sessions?x=1", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		if cookie != "" {
			req.Header.Set("Cookie", cookie)
		}
		res, err := noRedirect.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}

	if res := get("", ""); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	token := signToken(t, key, "k1", claims("ai-user"))
	res := get(token, "")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/sessions?x=1" ||
		res.Header.Get("Set-Cookie") != "dsh-auth-x=v" {
		t.Fatalf("exchange: %d %v", res.StatusCode, res.Header)
	}
	if res := get(token, "OauthHMAC-y=z; dsh-auth-x=v"); res.StatusCode != http.StatusOK {
		t.Fatalf("with cookie: %d", res.StatusCode)
	}
	if seen.Header.Get("Cookie") != "dsh-auth-x=v" || seen.Header.Get("Authorization") != "" {
		t.Fatalf("upstream headers %v", seen.Header)
	}
	// A refused dsh cookie is passed through, not exchanged again.
	if res := get(token, "dsh-auth-x=stale"); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stale cookie: %d", res.StatusCode)
	}
	if st := s.slots.Stats(); st.Running != 1 {
		t.Fatalf("slots %+v", st)
	}
}

func TestPublicAuthority(t *testing.T) {
	for in, want := range map[string]string{
		"https://dsh.example.com:8443/": "dsh.example.com:8443",
		"https://dsh.example.com":       "dsh.example.com",
	} {
		if got, err := publicAuthority(in); err != nil || got != want {
			t.Fatalf("%s: %q %v", in, got, err)
		}
	}
	if _, err := publicAuthority("dsh.example.com"); err == nil {
		t.Fatal("bare host accepted")
	}
}
