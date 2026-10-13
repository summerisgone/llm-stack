package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Per-user third-party credentials (docs/adr/0024). The dashboard's
// Connections panel writes a credential straight to OpenBao; pat-service's
// role can create, replace and delete it and read its metadata, never its
// value. Agents use it only through the /cred/ listener, which forwards to
// cred-gateway, the one component allowed to read it.

// connectors the dashboard offers, with the credential format it accepts.
var connectors = map[string]struct {
	label string
	token *regexp.Regexp
}{
	// Fine-grained PAT (stage 1); the GitHub App user-token flow replaces it
	// in stage 3.
	"github": {"GitHub", regexp.MustCompile(`^(github_pat_|ghp_)[A-Za-z0-9_]{20,250}$`)},
}

// connectionID is the OpenBao path key: the broker's profile id,
// sha256(sub) truncated to 16 hex chars (agent-broker auth.go).
func connectionID(sub string) string {
	sum := sha256.Sum256([]byte(sub))
	return hex.EncodeToString(sum[:])[:16]
}

var errNoSecret = errors.New("not found")

type baoWriter struct {
	addr, mount, role, jwtFile string
	client                     *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newBaoWriter() *baoWriter {
	addr := strings.TrimRight(os.Getenv("BAO_ADDR"), "/")
	if addr == "" {
		return nil
	}
	mount := os.Getenv("BAO_KV_MOUNT")
	if mount == "" {
		mount = "secret"
	}
	role := os.Getenv("BAO_ROLE")
	if role == "" {
		role = "pat-service"
	}
	return &baoWriter{addr: addr, mount: mount, role: role, jwtFile: saDir + "token", client: &http.Client{Timeout: 10 * time.Second}}
}

func (b *baoWriter) login(ctx context.Context) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.token != "" && time.Now().Before(b.expires) {
		return b.token, nil
	}
	jwt, err := os.ReadFile(b.jwtFile)
	if err != nil {
		return "", err
	}
	var out struct {
		Auth struct {
			ClientToken   string `json:"client_token"`
			LeaseDuration int    `json:"lease_duration"`
		} `json:"auth"`
	}
	if err := b.do(ctx, http.MethodPost, "/v1/auth/kubernetes/login", "", map[string]string{"role": b.role, "jwt": strings.TrimSpace(string(jwt))}, &out); err != nil {
		return "", fmt.Errorf("openbao login: %w", err)
	}
	b.token = out.Auth.ClientToken
	b.expires = time.Now().Add(time.Duration(out.Auth.LeaseDuration) * time.Second * 3 / 4)
	return b.token, nil
}

func (b *baoWriter) call(ctx context.Context, method, path string, body, out any) error {
	token, err := b.login(ctx)
	if err != nil {
		return err
	}
	err = b.do(ctx, method, "/v1/"+b.mount+path, token, body, out)
	if err != nil && !errors.Is(err, errNoSecret) {
		b.mu.Lock()
		b.token = ""
		b.mu.Unlock()
	}
	return err
}

func (b *baoWriter) do(ctx context.Context, method, path, token string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, b.addr+path, rd)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNoSecret
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, bytes.TrimSpace(msg))
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type connection struct {
	Connector string     `json:"connector"`
	Label     string     `json:"label"`
	Connected bool       `json:"connected"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

func (a *app) listConnections(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if a.bao == nil {
		http.Error(w, "connections are not available on this deployment", http.StatusServiceUnavailable)
		return
	}
	out := []connection{}
	for name, c := range connectors {
		var meta struct {
			Data struct {
				UpdatedTime time.Time `json:"updated_time"`
			} `json:"data"`
		}
		conn := connection{Connector: name, Label: c.label}
		err := a.bao.call(r.Context(), http.MethodGet, "/metadata/users/"+connectionID(s.Subject)+"/"+name, nil, &meta)
		switch {
		case err == nil:
			conn.Connected, conn.UpdatedAt = true, &meta.Data.UpdatedTime
		case !errors.Is(err, errNoSecret):
			log.Printf("connections: %v", err)
			http.Error(w, "credential store unavailable", http.StatusServiceUnavailable)
			return
		}
		out = append(out, conn)
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *app) putConnection(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	name := r.PathValue("connector")
	c, known := connectors[name]
	if !known {
		http.NotFound(w, r)
		return
	}
	if a.bao == nil {
		http.Error(w, "connections are not available on this deployment", http.StatusServiceUnavailable)
		return
	}
	var input struct {
		Token string `json:"token"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&input) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	input.Token = strings.TrimSpace(input.Token)
	if !c.token.MatchString(input.Token) {
		http.Error(w, "not a "+c.label+" personal access token", 400)
		return
	}
	body := map[string]any{"data": map[string]string{"token": input.Token, "kind": "pat"}}
	if err := a.bao.call(r.Context(), http.MethodPost, "/data/users/"+connectionID(s.Subject)+"/"+name, body, nil); err != nil {
		log.Printf("connections: %v", err)
		http.Error(w, "credential store unavailable", http.StatusServiceUnavailable)
		return
	}
	log.Printf("connection stored user=%s connector=%s", connectionID(s.Subject), name)
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) deleteConnection(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	name := r.PathValue("connector")
	if _, known := connectors[name]; !known {
		http.NotFound(w, r)
		return
	}
	if a.bao == nil {
		http.Error(w, "connections are not available on this deployment", http.StatusServiceUnavailable)
		return
	}
	// Deleting the metadata removes every version.
	err := a.bao.call(r.Context(), http.MethodDelete, "/metadata/users/"+connectionID(s.Subject)+"/"+name, nil, nil)
	if err != nil && !errors.Is(err, errNoSecret) {
		log.Printf("connections: %v", err)
		http.Error(w, "credential store unavailable", http.StatusServiceUnavailable)
		return
	}
	log.Printf("connection deleted user=%s connector=%s", connectionID(s.Subject), name)
	w.WriteHeader(http.StatusNoContent)
}

// credProxy serves /cred/<connector>/... on its own listener, which no
// HTTPRoute references and a NetworkPolicy opens to the agents namespace
// only: a route on the 8080 mux would be public as /platform/cred/
// (docs/adr/0024 decision 3). Agents authenticate with their PAT.
func (a *app) credProxy(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/cred/") {
		http.NotFound(w, r)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(token, "sk-") || strings.TrimSpace(token) != token {
		http.Error(w, "agent PAT required", http.StatusUnauthorized)
		return
	}
	_, owner, ownerName, _, _, err := a.lookupPAT(r.Context(), token)
	if errors.Is(err, errDatabaseUnavailable) {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if err != nil {
		http.Error(w, "invalid PAT", http.StatusUnauthorized)
		return
	}
	target, err := url.Parse(a.cfg.credGatewayURL)
	if err != nil {
		http.Error(w, "credential gateway unavailable", http.StatusBadGateway)
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = ""
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Set("X-User-Id", owner)
			pr.Out.Header.Set("X-User-Name", ownerName)
		},
		FlushInterval: -1,
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			log.Printf("cred-gateway: %v", err)
			http.Error(w, "credential gateway unavailable", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

func newCredMux(a *app) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/cred/", a.credProxy)
	return mux
}
