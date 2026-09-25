package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// /mcp/<name>/ proxies to the MCP server cfg.mcpServers[name]
// (docs/adr/0017-web-search-mcp-openserp-kagent.md section 3, docs/adr/0018
// section 5). Callers are Hermes agents (their PAT) and Open WebUI (the chat
// user's Keycloak access token, auth_type system_oauth). Upstreams trust the
// identity headers set here because their NetworkPolicy admits pat-service
// only.

// mcpMaxInspectBody bounds how much of a request is read to find the tool
// name; the upstreams' own request limits are larger, so a longer body is
// forwarded untouched and counted as tool "other".
const mcpMaxInspectBody = 64 << 10

type windowCounter interface {
	IncrWindow(ctx context.Context, key string, window time.Duration) (int64, error)
}

type mcpCaller struct {
	subject, name, tokenID string
}

// mcpServer is one /mcp/<name>/ upstream.
type mcpServer struct {
	name, url string
}

func (a *app) mcpProxy(w http.ResponseWriter, r *http.Request) {
	name, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/mcp/"), "/")
	upstream, ok := a.cfg.mcpServers[name]
	if !ok || !strings.HasPrefix(r.URL.Path, "/mcp/"+name+"/") {
		http.NotFound(w, r)
		return
	}
	caller, status := a.mcpAuthenticate(r)
	if status != http.StatusOK {
		http.Error(w, http.StatusText(status), status)
		return
	}
	a.forwardMCP(w, r, mcpServer{name, upstream}, caller)
}

// mcpAuthenticate accepts a PAT (same lookup as /v1/) or a Keycloak access
// token with the role Open WebUI admission requires.
func (a *app) mcpAuthenticate(r *http.Request) (mcpCaller, int) {
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || token == "" || strings.TrimSpace(token) != token {
		return mcpCaller{}, http.StatusUnauthorized
	}
	if strings.HasPrefix(token, "sk-") {
		id, owner, ownerName, _, err := a.lookupPAT(r.Context(), token)
		if errors.Is(err, errDatabaseUnavailable) {
			return mcpCaller{}, http.StatusServiceUnavailable
		}
		if err != nil {
			return mcpCaller{}, http.StatusUnauthorized
		}
		return mcpCaller{subject: owner, name: ownerName, tokenID: id}, http.StatusOK
	}
	cl, err := a.verifyJWT(r.Context(), token)
	if err != nil {
		return mcpCaller{}, http.StatusUnauthorized
	}
	if !hasAIUserRole(cl.RealmAccess.Roles) {
		return mcpCaller{}, http.StatusForbidden
	}
	return mcpCaller{subject: cl.Subject, name: cl.PreferredUsername}, http.StatusOK
}

func (a *app) forwardMCP(w http.ResponseWriter, r *http.Request, srv mcpServer, caller mcpCaller) {
	var tool string
	r.Body, tool = mcpToolName(r)
	record := func(status int) {
		a.qos.Metrics().MCPCallsTotal.WithLabelValues(caller.subject, srv.name, tool, strconv.Itoa(status)).Inc()
	}
	// Only tool calls count against the limit: initialize and tools/list are
	// protocol overhead a client repeats on every reconnect.
	if tool != "none" && a.mcpLimited(r.Context(), srv.name, caller.subject) {
		record(http.StatusTooManyRequests)
		w.Header().Set("Retry-After", "60")
		http.Error(w, "MCP call rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	target, err := url.Parse(srv.url)
	if err != nil {
		http.Error(w, srv.name+" unavailable", http.StatusBadGateway)
		return
	}
	prefix := "/mcp/" + srv.name
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme = target.Scheme
			pr.Out.URL.Host = target.Host
			rest := strings.TrimPrefix(pr.In.URL.Path, prefix)
			pr.Out.URL.Path = target.Path + rest
			// An upstream mounted at a path (repowise: /mcp) is reached at
			// that exact path, not with a trailing slash it would redirect.
			if target.Path != "" && rest == "/" {
				pr.Out.URL.Path = target.Path
			}
			pr.Out.URL.RawPath = ""
			pr.Out.Host = ""
			pr.Out.Header = http.Header{}
			copyRequestHeaders(pr.Out.Header, pr.In.Header)
			// The pat-service dashboard session is not for the upstream.
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("X-User-Id", caller.subject)
			name := caller.name
			if name == "" {
				name = caller.subject
			}
			pr.Out.Header.Set("X-User-Name", name)
			if caller.tokenID != "" {
				pr.Out.Header.Set("X-Pat-Token-Id", caller.tokenID)
			}
		},
		// Streamable HTTP may answer with an SSE stream; pass it through
		// as it arrives.
		FlushInterval: -1,
		ModifyResponse: func(resp *http.Response) error {
			record(resp.StatusCode)
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("mcp %s: %v", srv.name, err)
			record(http.StatusBadGateway)
			http.Error(w, srv.name+" unavailable", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// mcpLimited is a fixed one-minute window per (server, user). Like the rest
// of qos it fails open: an unreachable Valkey must not stop the tools.
func (a *app) mcpLimited(ctx context.Context, server, subject string) bool {
	if a.mcpLimiter == nil || a.cfg.mcpCallsPerMinute <= 0 {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, a.cfg.qosFingerprintTimeout)
	defer cancel()
	key := fmt.Sprintf("mcp:rl:%s:%s:%d", server, subject, time.Now().Unix()/60)
	n, err := a.mcpLimiter.IncrWindow(ctx, key, 2*time.Minute)
	if err != nil {
		return false
	}
	return n > int64(a.cfg.mcpCallsPerMinute)
}

// mcpToolName reads at most mcpMaxInspectBody bytes of a JSON-RPC request
// and returns a reader that reproduces the body exactly, plus the tool:
// web_search|fetch_url|other for tools/call, "none" for anything else.
func mcpToolName(r *http.Request) (io.ReadCloser, string) {
	if r.Method != http.MethodPost || r.Body == nil {
		return r.Body, "none"
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, mcpMaxInspectBody+1))
	restored := io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))
	if err != nil || len(head) > mcpMaxInspectBody {
		return restored, "other"
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal(head, &msg) != nil {
		// Batches and malformed bodies: counted, and limited, as a call.
		return restored, "other"
	}
	if msg.Method != "tools/call" {
		return restored, "none"
	}
	switch msg.Params.Name {
	case "web_search", "fetch_url":
		return restored, msg.Params.Name
	}
	return restored, "other"
}
