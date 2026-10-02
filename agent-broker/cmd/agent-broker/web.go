package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
)

// The per-user dsh web UI (docs/adr/0020). Envoy's OIDC filter on the dsh
// host forwards the user's Keycloak access token; the broker sends the
// request to that user's dsh agent pod, starting it like a chat would, and
// proxies HTTP and WebSocket to dsh web on DshWebPort. dsh authenticates
// browsers with its own cookie, minted from a per-process launch token: on a
// 401 the broker fetches the token from agent-adapter and performs the
// exchange itself, so the token never reaches the browser.

const (
	DshWebPort      = 3080
	dshCookiePrefix = "dsh-auth-"
)

type webProxy struct {
	s *server
	// authority is the public host[:port] of the dsh origin. dsh binds its
	// cookies to the Host it sees and requires Origin == Host, so every
	// upstream request carries it.
	authority string
	port      int // dsh web's port in the pod, DshWebPort
	client    *http.Client
}

func (p *webProxy) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, p.s.slots.Stats())
	})
	mux.Handle("/", p)
	return mux
}

func (p *webProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	u, err := p.s.auth.Verify(ctx, r.Header.Get("Authorization"))
	switch {
	case errors.Is(err, errForbidden):
		http.Error(w, "the ai-user or ai-admin role is required", http.StatusForbidden)
		return
	case errors.Is(err, errUnauthenticated):
		http.Error(w, "a valid Keycloak access token is required", http.StatusUnauthorized)
		return
	case err != nil:
		slog.Error("token verification", "err", err)
		http.Error(w, "identity provider unreachable", http.StatusServiceUnavailable)
		return
	}
	ref := AgentRef{Runtime: "dsh", ID: u.ID}
	if _, err := p.s.backend.EnsureProfile(ctx, u, ref); err != nil {
		slog.Error("ensure profile", "agent", ref.String(), "err", err)
		http.Error(w, "cannot create the agent profile", http.StatusInternalServerError)
		return
	}
	agent, _, err := p.s.slots.Acquire(ctx, u, ref, nil)
	if err != nil {
		if errors.Is(err, errNoSlot) {
			http.Error(w, "all agent slots are busy, try again in a minute", http.StatusServiceUnavailable)
		} else {
			http.Error(w, "the agent could not be started: "+err.Error(), http.StatusBadGateway)
		}
		return
	}
	released := false
	release := func() {
		if !released {
			released = true
			p.s.slots.Release(ref)
		}
	}
	defer release()
	if r.Header.Get("Upgrade") != "" {
		// An open tab's event stream must not pin the slot: activity is the
		// HTTP traffic, and an evicted agent just drops the socket.
		release()
	}
	target, err := url.Parse(agent.Endpoint)
	if err != nil {
		http.Error(w, "bad agent endpoint", http.StatusBadGateway)
		return
	}
	target.Host = net.JoinHostPort(target.Hostname(), strconv.Itoa(p.port))
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = p.authority
			pr.Out.Header.Del("Authorization")
			keepDshCookies(pr.Out.Header)
		},
		ModifyResponse: func(res *http.Response) error {
			// A request that already had a dsh cookie is not retried: no
			// redirect loop when the cookie is refused for another reason.
			if res.StatusCode != http.StatusUnauthorized || (r.Method != http.MethodGet && r.Method != http.MethodHead) ||
				r.Header.Get("Upgrade") != "" || strings.Contains(r.Header.Get("Cookie"), dshCookiePrefix) {
				return nil
			}
			cookie, err := p.exchange(res.Request.Context(), agent, target)
			if err != nil {
				slog.Warn("dsh web token exchange", "agent", ref.String(), "err", err)
				return nil
			}
			// Same URL again, now with dsh's session cookie.
			res.Body.Close()
			res.StatusCode, res.Status = http.StatusSeeOther, "303 See Other"
			res.Header = http.Header{"Location": {r.URL.RequestURI()}, "Set-Cookie": cookie,
				"Cache-Control": {"no-store"}}
			res.Body, res.ContentLength = http.NoBody, 0
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("dsh web proxy", "agent", ref.String(), "err", err)
			http.Error(w, "the agent's web UI is not reachable", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// keepDshCookies forwards only dsh's own cookies; Envoy's OIDC cookies (the
// user's tokens) stay at the gateway.
func keepDshCookies(h http.Header) {
	var kept []string
	for _, line := range h.Values("Cookie") {
		for _, c := range strings.Split(line, ";") {
			if strings.HasPrefix(strings.TrimSpace(c), dshCookiePrefix) {
				kept = append(kept, strings.TrimSpace(c))
			}
		}
	}
	h.Del("Cookie")
	if len(kept) > 0 {
		h.Set("Cookie", strings.Join(kept, "; "))
	}
}

// exchange asks agent-adapter for dsh web's launch token and redeems it on
// GET /?token=, returning the Set-Cookie lines dsh answered with.
func (p *webProxy) exchange(ctx context.Context, agent *Agent, target *url.URL) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, agent.Endpoint+"/web-token", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+agent.APIKey)
	res, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("web-token: %s", res.Status)
	}
	var body struct {
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&body); err != nil || body.Token == "" {
		return nil, fmt.Errorf("web-token: no token")
	}
	login := *target
	login.Path, login.RawQuery = "/", url.Values{"token": {body.Token}}.Encode()
	req, err = http.NewRequestWithContext(ctx, http.MethodGet, login.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Host = p.authority
	noRedirect := *p.client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err = noRedirect.Do(req)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	cookie := res.Header.Values("Set-Cookie")
	if len(cookie) == 0 {
		return nil, fmt.Errorf("token exchange: %s without a cookie", res.Status)
	}
	return cookie, nil
}

// publicAuthority is the host[:port] of an origin such as
// https://dsh.example.com:8443/ (a trailing slash is fine).
func publicAuthority(origin string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("DSH_PUBLIC_ORIGIN %q is not an absolute URL", origin)
	}
	return u.Host, nil
}
