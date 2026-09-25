// pat-service issues self-service personal access tokens and proxies the
// OpenAI-compatible API only after a live token lookup.
package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/airgap-ai-stack/pat-service/internal/qos"
)

const (
	cookieName      = "pat_session"
	loginCookieName = "pat_login"
)

// dashboardAssets is the production build of the dashboard. Keeping it in
// the binary makes the service a single deployable and avoids a second web
// server or a runtime dependency on a Node toolchain.
//
//go:embed assets/*
var dashboardAssets embed.FS

type config struct {
	databaseURL     string
	hashKey         []byte
	cookieKey       []byte
	issuer          string
	internalIssuer  string
	clientID        string
	redirectURL     string
	gatewayURL      string
	gatewayClientID string
	gatewaySecret   string
	secureCookies   bool
	// urlPrefix is the public path under which the dashboard is served.
	// It is stripped by the Gateway's URLRewrite filter before requests
	// reach this service, so we add it back to every Location header and
	// inline it into the dashboard HTML so the React app builds correct
	// fetch() URLs against `/<prefix>/api/...` rather than `/api/...`.
	urlPrefix string
	// logClientShape is TASK-qos-fair-share.md Stage 0.4 diagnostic
	// instrumentation: log which header names and top-level body keys real
	// coding-agent clients actually send, to confirm or refute whether any
	// of them can carry a session-identifying header before Stage 3 commits
	// to a server-side-only fingerprint. Remove once that data is collected.
	logClientShape bool
	// webuiURL is the public origin of Open WebUI, inlined into the
	// dashboard so the React app's "Open WebUI" link points at the right
	// place for this deployment. Read from WEBUI_URL; the JS falls back to
	// the local-mac dev host when this is empty.
	webuiURL string
	// valkeyAddr is TASK-qos-fair-share.md Stage 3's session-tracking
	// state store (the same envoy-ratelimit-valkey instance the rate
	// limiter already uses -- docs/adr/0008-per-user-fair-share.md
	// "State: Valkey, not in-process memory").
	valkeyAddr string
	// qosSessionTTL is the sliding TTL on chain links and step counters
	// (TASK-qos-fair-share.md §3.3 default: 30 minutes).
	qosSessionTTL time.Duration
	// qosFingerprintMaxBody bounds how much of a chat-completion body is
	// read for chain hashing; requests over this are proxied unaffected,
	// just without a session match (§3, Stage 3's degradation rule).
	// Defaults to the AI Gateway's own request-buffer ceiling (4Mi) since
	// nothing larger ever arrives anyway.
	qosFingerprintMaxBody int64
	// qosFingerprintTimeout bounds the Valkey round-trip for a single
	// Observe call so a slow/unreachable Valkey degrades a request's
	// session match instead of adding to its latency.
	qosFingerprintTimeout time.Duration
	// qosWarmTTL is TASK-qos-fair-share.md §4.2's WARM_TTL: a session
	// dispatched within this long ago is eligible for the warm band
	// (default 120s).
	qosWarmTTL time.Duration
	// qosDemoteAfterSteps is §4.2's N: a session that has run this many
	// consecutive steps without yielding is demoted on its next request
	// (default 8).
	qosDemoteAfterSteps int64
	// qosCostAlpha and qosCostBeta are §4.4's cost coefficients: seconds
	// of uncached-prefill and decode cost per token, respectively.
	// Calibrated 2026-09-07 against qwen-3.8-27b on vLLM 0.27.1 with
	// prefix caching, fp8 KV cache, and TRITON_ATTN (tests/qos/baseline.md
	// §0.7); recalibrate if the model, engine, or those flags change.
	qosCostAlpha float64
	qosCostBeta  float64
	// qosSpendWindow is how long a user's recent spend (§4.4) is
	// remembered before it decays back to zero from inactivity.
	qosSpendWindow time.Duration
	// qosSpendDemoteThreshold is how far ahead of the least-spending
	// other active user someone must be, in cost units, before their
	// priority band is demoted regardless of session state. Roughly one
	// heavy cold-prefill request's worth of lead at the calibrated
	// coefficients; a first cut pending real spend-distribution data from
	// Stage 4's dashboards.
	qosSpendDemoteThreshold float64
	// qosUsageMaxBody bounds how much of a chat-completion response is
	// read to extract usage for cost accounting; a response over this is
	// proxied unaffected, just without a spend update -- degradation, not
	// a failure, same rule as the request-body fingerprint cap.
	qosUsageMaxBody int64
	// qosEventTimeout bounds the qos_events insert (and, for streaming
	// responses, the RecordCost call alongside it) so a slow Postgres
	// degrades that one usage record instead of adding to response
	// latency. Separate from qosFingerprintTimeout: this writes to
	// pat-db, not Valkey, and runs after the response has already been
	// fully proxied, so it can afford a looser budget.
	qosEventTimeout time.Duration
	// pricingFile is an optional path to a JSON file mapping model name
	// to price per 1K prompt/completion tokens (see pricing.go). Empty or
	// unreadable: pricing stays empty and every qos_events row is costed
	// at 0, same degrade-don't-fail rule as everything else here.
	pricingFile string
	// qosMonthlyLimit is the /api/usage/limit preview's budget for the
	// current calendar month, in pricing.json's currency. Zero means no
	// limit is configured yet. This is display-only -- nothing in this
	// service enforces it (TASK-qos-fair-share.md Stage 3 step 5's hard
	// per-user ceiling remains out of scope); it exists so the /platform
	// dashboard can show spend-vs-limit ahead of that policy being
	// decided.
	qosMonthlyLimit float64
	// hermesNamespace and hermesTTLDays configure dashboard issuance of the
	// Hermes agent's inference key (hermes.go, docs/adr/0014 section 8).
	hermesNamespace string
	hermesTTLDays   int
	// webSearchEnabled opens /mcp/web-search/ (mcp.go, docs/adr/0017);
	// WEB_SEARCH_ENABLED from .env through the airgap-runtime Secret.
	webSearchEnabled  bool
	webSearchMCPURL   string
	mcpCallsPerMinute int
}

type app struct {
	cfg       config
	db        *pgxpool.Pool
	http      *http.Client
	proxyHTTP *http.Client
	keys      keySet
	gateway   gatewayToken
	qos       *qos.Tracker
	pricing   pricing
	hermes    *hermesKube
	// mcpLimiter backs the per-user MCP call limit (mcp.go).
	mcpLimiter windowCounter
}

type session struct {
	Subject  string   `json:"sub"`
	Username string   `json:"preferred_username"`
	Roles    []string `json:"roles"`
	Expires  int64    `json:"exp"`
}

type loginState struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Expires  int64  `json:"exp"`
}

type claims struct {
	Issuer            string `json:"iss"`
	Subject           string `json:"sub"`
	PreferredUsername string `json:"preferred_username"`
	Expires           int64  `json:"exp"`
	RealmAccess       struct {
		Roles []string `json:"roles"`
	} `json:"realm_access"`
}

type jwk struct {
	KID string `json:"kid"`
	KTY string `json:"kty"`
	N   string `json:"n"`
	E   string `json:"e"`
}

type keySet struct {
	mu      sync.Mutex
	keys    map[string]*rsa.PublicKey
	expires time.Time
}

type gatewayToken struct {
	mu      sync.Mutex
	value   string
	expires time.Time
}

type tokenRecord struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// CostAmount is this token's lifetime spend (sum of qos_events.cost_amount
	// for requests authenticated with it), in a.pricing.Currency -- the
	// /platform token table's per-key usage total.
	CostAmount float64 `json:"cost_amount"`
	// IssuedBy is "user" or "hermes" (the Hermes agent's key, hermes.go).
	IssuedBy string `json:"issued_by"`
}

func main() {
	cfg, err := loadConfig()
	if err != nil {
		log.Fatal(err)
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, cfg.databaseURL)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := db.Ping(ctx); err != nil {
		log.Fatal(err)
	}
	registry := prometheus.NewRegistry()
	qosMetrics := qos.NewMetrics(registry)
	qosStore := qos.NewRedisStore(cfg.valkeyAddr)
	a := &app{
		cfg:        cfg,
		db:         db,
		http:       &http.Client{Timeout: 30 * time.Second},
		proxyHTTP:  &http.Client{},
		qos:        qos.NewTracker(qosStore, qosMetrics, cfg.qosSessionTTL, cfg.qosWarmTTL, cfg.qosDemoteAfterSteps, cfg.qosCostAlpha, cfg.qosCostBeta, cfg.qosSpendDemoteThreshold, cfg.qosSpendWindow),
		pricing:    loadPricing(cfg.pricingFile),
		hermes:     newHermesKube(cfg.hermesNamespace),
		mcpLimiter: qosStore,
	}
	if err := a.migrate(ctx); err != nil {
		log.Fatal(err)
	}
	go qosMetrics.RunActiveSweep(ctx, qosStore, cfg.qosSessionTTL, 30*time.Second)

	metricsMux := http.NewServeMux()
	metricsMux.Handle("GET /metrics", promhttp.HandlerFor(registry, promhttp.HandlerOpts{}))
	metricsServer := &http.Server{Addr: ":9090", Handler: metricsMux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		// Internal-only: no Service/HTTPRoute exposes this port through the
		// edge (TASK-qos-fair-share.md Stage 4: "не публиковать наружу").
		log.Printf("PAT service metrics listening on %s", metricsServer.Addr)
		log.Fatal(metricsServer.ListenAndServe())
	}()

	mux := newMux(a)
	server := &http.Server{Addr: ":8080", Handler: securityHeaders(mux), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	log.Printf("PAT service listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func newMux(a *app) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", a.health)
	mux.HandleFunc("GET /", a.dashboard)
	mux.Handle("GET /assets/", http.FileServer(http.FS(dashboardAssets)))
	mux.HandleFunc("GET /auth/login", a.login)
	mux.HandleFunc("GET /auth/callback", a.callback)
	mux.HandleFunc("POST /auth/logout", a.logout)
	mux.HandleFunc("GET /api/tokens", a.listTokens)
	mux.HandleFunc("POST /api/tokens", a.createToken)
	mux.HandleFunc("POST /api/tokens/", a.revokeToken)
	mux.HandleFunc("POST /api/hermes-token", a.issueHermesToken)
	mux.HandleFunc("GET /api/usage/daily", a.usageDaily)
	mux.HandleFunc("GET /api/usage/sessions", a.usageSessions)
	mux.HandleFunc("GET /api/usage/limit", a.usageLimit)
	// ServeMux rejects a method-agnostic /v1/ route alongside GET /. OpenAI
	// uses these methods; explicit registrations also make the public surface
	// intentionally narrow.
	mux.HandleFunc("GET /v1/", a.proxy)
	mux.HandleFunc("POST /v1/", a.proxy)
	mux.HandleFunc("DELETE /v1/", a.proxy)
	mux.HandleFunc("GET /mcp/web-search/", a.mcpProxy)
	mux.HandleFunc("POST /mcp/web-search/", a.mcpProxy)
	mux.HandleFunc("DELETE /mcp/web-search/", a.mcpProxy)
	return mux
}

func loadConfig() (config, error) {
	read := func(name string) (string, error) {
		v := os.Getenv(name)
		if v == "" {
			return "", fmt.Errorf("%s is required", name)
		}
		return v, nil
	}
	databaseURL, err := read("DATABASE_URL")
	if err != nil {
		return config{}, err
	}
	hash, err := read("PAT_HASH_KEY")
	if err != nil {
		return config{}, err
	}
	cookie, err := read("PAT_COOKIE_KEY")
	if err != nil {
		return config{}, err
	}
	issuer, err := read("OIDC_ISSUER")
	if err != nil {
		return config{}, err
	}
	internal, err := read("OIDC_INTERNAL_ISSUER")
	if err != nil {
		return config{}, err
	}
	clientID, err := read("OIDC_CLIENT_ID")
	if err != nil {
		return config{}, err
	}
	redirect, err := read("OIDC_REDIRECT_URL")
	if err != nil {
		return config{}, err
	}
	gatewayURL, err := read("AI_GATEWAY_URL")
	if err != nil {
		return config{}, err
	}
	gatewayClientID, err := read("AI_GATEWAY_CLIENT_ID")
	if err != nil {
		return config{}, err
	}
	gatewaySecret, err := read("AI_GATEWAY_CLIENT_SECRET")
	if err != nil {
		return config{}, err
	}
	if len(hash) < 32 || len(cookie) < 32 {
		return config{}, errors.New("PAT_HASH_KEY and PAT_COOKIE_KEY must each be at least 32 bytes")
	}
	prefix := strings.TrimRight(os.Getenv("URL_PREFIX"), "/")
	logClientShape := os.Getenv("QOS_LOG_CLIENT_SHAPE") == "true"
	webui := strings.TrimRight(os.Getenv("WEBUI_URL"), "/")
	valkeyAddr, err := read("VALKEY_ADDR")
	if err != nil {
		return config{}, err
	}
	qosSessionTTL := time.Duration(readIntEnv("QOS_SESSION_TTL_SECONDS", 1800)) * time.Second
	qosFingerprintMaxBody := int64(readIntEnv("QOS_FINGERPRINT_MAX_BODY_BYTES", 4<<20))
	qosFingerprintTimeout := time.Duration(readIntEnv("QOS_FINGERPRINT_TIMEOUT_MS", 200)) * time.Millisecond
	qosWarmTTL := time.Duration(readIntEnv("QOS_WARM_TTL_SECONDS", 120)) * time.Second
	qosDemoteAfterSteps := int64(readIntEnv("QOS_DEMOTE_AFTER_STEPS", 8))
	qosCostAlpha := readFloatEnv("QOS_COST_ALPHA_PER_TOKEN", 9.4e-5)
	qosCostBeta := readFloatEnv("QOS_COST_BETA_PER_TOKEN", 0.0149)
	qosSpendWindow := time.Duration(readIntEnv("QOS_SPEND_WINDOW_SECONDS", 600)) * time.Second
	qosSpendDemoteThreshold := readFloatEnv("QOS_SPEND_DEMOTE_THRESHOLD", 5.0)
	qosUsageMaxBody := int64(readIntEnv("QOS_USAGE_MAX_BODY_BYTES", 1<<20))
	qosEventTimeout := time.Duration(readIntEnv("QOS_EVENT_TIMEOUT_MS", 1000)) * time.Millisecond
	pricingFile := os.Getenv("QOS_PRICING_FILE")
	qosMonthlyLimit := readFloatEnv("QOS_MONTHLY_LIMIT", 0)
	hermesNamespace := os.Getenv("HERMES_NAMESPACE")
	if hermesNamespace == "" {
		hermesNamespace = "hermes-agents"
	}
	hermesTTLDays := readIntEnv("HERMES_PAT_TTL_DAYS", 7)
	webSearchEnabled := os.Getenv("WEB_SEARCH_ENABLED") == "true"
	webSearchMCPURL := strings.TrimRight(os.Getenv("WEB_SEARCH_MCP_URL"), "/")
	if webSearchMCPURL == "" {
		webSearchMCPURL = "http://web-search-mcp.airgap-ai-stack.svc.cluster.local:8080"
	}
	mcpCallsPerMinute := readIntEnv("MCP_CALLS_PER_MINUTE", 30)
	return config{databaseURL, []byte(hash), []byte(cookie), issuer, internal, clientID, redirect, gatewayURL, gatewayClientID, gatewaySecret, strings.HasPrefix(issuer, "https://"), prefix, logClientShape, webui, valkeyAddr, qosSessionTTL, qosFingerprintMaxBody, qosFingerprintTimeout, qosWarmTTL, qosDemoteAfterSteps, qosCostAlpha, qosCostBeta, qosSpendWindow, qosSpendDemoteThreshold, qosUsageMaxBody, qosEventTimeout, pricingFile, qosMonthlyLimit, hermesNamespace, hermesTTLDays, webSearchEnabled, webSearchMCPURL, mcpCallsPerMinute}, nil
}

// readIntEnv reads an optional integer threshold, falling back to def when
// the variable is unset or unparsable. Every QoS threshold has a default
// here and can be overridden without a rebuild, per
// TASK-qos-fair-share.md §4.4's "no magic numbers in code" rule.
func readIntEnv(name string, def int) int {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// readFloatEnv is readIntEnv's counterpart for the cost model's
// non-integer coefficients and thresholds.
func readFloatEnv(name string, def float64) float64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

func (a *app) migrate(ctx context.Context) error {
	_, err := a.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS personal_access_tokens (
 id text PRIMARY KEY, owner_subject text NOT NULL, owner_name text NOT NULL DEFAULT '', token_hash bytea NOT NULL UNIQUE,
 token_prefix text NOT NULL, name text NOT NULL, created_at timestamptz NOT NULL DEFAULT now(),
 expires_at timestamptz, revoked_at timestamptz, last_used_at timestamptz);
 ALTER TABLE personal_access_tokens ADD COLUMN IF NOT EXISTS owner_name text NOT NULL DEFAULT '';
 CREATE INDEX IF NOT EXISTS personal_access_tokens_owner_active_idx
 ON personal_access_tokens (owner_subject, created_at DESC) WHERE revoked_at IS NULL;
 CREATE TABLE IF NOT EXISTS qos_events (
 id bigserial PRIMARY KEY, owner_subject text NOT NULL, owner_name text NOT NULL DEFAULT '',
 session_id text NOT NULL DEFAULT '', model text NOT NULL, band text NOT NULL,
 prompt_tokens bigint NOT NULL DEFAULT 0, cached_tokens bigint NOT NULL DEFAULT 0, completion_tokens bigint NOT NULL DEFAULT 0,
 cost_amount numeric(12,4) NOT NULL DEFAULT 0, created_at timestamptz NOT NULL DEFAULT now());
 CREATE INDEX IF NOT EXISTS qos_events_owner_day_idx ON qos_events (owner_subject, created_at);
 CREATE INDEX IF NOT EXISTS qos_events_owner_session_idx ON qos_events (owner_subject, session_id, created_at);
 ALTER TABLE qos_events ADD COLUMN IF NOT EXISTS token_id text NOT NULL DEFAULT '';
 ALTER TABLE qos_events ADD COLUMN IF NOT EXISTS token_name text NOT NULL DEFAULT '';
 CREATE INDEX IF NOT EXISTS qos_events_token_idx ON qos_events (token_id);
 ALTER TABLE personal_access_tokens ADD COLUMN IF NOT EXISTS issued_by text NOT NULL DEFAULT 'user';`)
	return err
}

func (a *app) health(w http.ResponseWriter, r *http.Request) {
	if err := a.db.Ping(r.Context()); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) dashboard(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.currentSession(r); !ok {
		http.Redirect(w, r, a.cfg.urlPrefix+"/auth/login", http.StatusFound)
		return
	}
	page, err := dashboardAssets.ReadFile("assets/index.html")
	if err != nil {
		log.Printf("read embedded dashboard: %v", err)
		http.Error(w, "dashboard unavailable", http.StatusServiceUnavailable)
		return
	}
	// The dashboard SPA hits `/api/tokens` and `/auth/logout` directly.
	// Inject the public URL prefix into the rendered HTML so the React
	// client-side fetch() calls hit `<prefix>/api/tokens` etc, and the
	// public Open WebUI origin for the "Open WebUI" link.
	page = []byte(strings.Replace(string(page), "{{URL_PREFIX}}", a.cfg.urlPrefix, 1))
	page = []byte(strings.Replace(string(page), "{{WEBUI_URL}}", a.cfg.webuiURL, 1))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(page)
}

func (a *app) login(w http.ResponseWriter, r *http.Request) {
	state, err := randomURL(24)
	if err != nil {
		http.Error(w, "random source unavailable", 500)
		return
	}
	verifier, err := randomURL(48)
	if err != nil {
		http.Error(w, "random source unavailable", 500)
		return
	}
	login := loginState{State: state, Verifier: verifier, Expires: time.Now().Add(10 * time.Minute).Unix()}
	if err := a.setSignedCookie(w, loginCookieName, login, 600); err != nil {
		http.Error(w, "session error", 500)
		return
	}
	challenge := sha256.Sum256([]byte(verifier))
	q := url.Values{"client_id": {a.cfg.clientID}, "redirect_uri": {a.cfg.redirectURL}, "response_type": {"code"}, "scope": {"openid profile email roles"}, "state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])}, "code_challenge_method": {"S256"}}
	http.Redirect(w, r, a.cfg.issuer+"/protocol/openid-connect/auth?"+q.Encode(), http.StatusFound)
}

func (a *app) callback(w http.ResponseWriter, r *http.Request) {
	var login loginState
	if !a.readSignedCookie(r, loginCookieName, &login) || login.Expires < time.Now().Unix() || r.URL.Query().Get("state") != login.State {
		http.Error(w, "invalid login state", http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	if code == "" {
		http.Error(w, "missing authorization code", 400)
		return
	}
	form := url.Values{"grant_type": {"authorization_code"}, "client_id": {a.cfg.clientID}, "redirect_uri": {a.cfg.redirectURL}, "code": {code}, "code_verifier": {login.Verifier}}
	resp, err := a.http.PostForm(a.cfg.internalIssuer+"/protocol/openid-connect/token", form)
	if err != nil {
		http.Error(w, "SSO unavailable", 502)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		http.Error(w, "SSO rejected login", 401)
		return
	}
	var result struct {
		AccessToken string `json:"access_token"`
	}
	if json.NewDecoder(resp.Body).Decode(&result) != nil {
		http.Error(w, "invalid SSO response", 502)
		return
	}
	cl, err := a.verifyJWT(r.Context(), result.AccessToken)
	if err != nil || !hasAIUserRole(cl.RealmAccess.Roles) {
		http.Error(w, "AI Stack role is required", http.StatusForbidden)
		return
	}
	if err := a.setSignedCookie(w, cookieName, session{
		Subject:  cl.Subject,
		Username: cl.PreferredUsername,
		Roles:    cl.RealmAccess.Roles,
		Expires:  cl.Expires,
	}, int(time.Until(time.Unix(cl.Expires, 0)).Seconds())); err != nil {
		http.Error(w, "session error", 500)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.cfg.secureCookies, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, a.cfg.urlPrefix+"/", http.StatusFound)
}

func (a *app) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: cookieName, Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.cfg.secureCookies, SameSite: http.SameSiteLaxMode})
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) listTokens(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	rows, err := a.db.Query(r.Context(), `
		SELECT t.id, t.name, t.token_prefix, t.created_at, t.expires_at, t.revoked_at, t.last_used_at,
		       COALESCE(SUM(e.cost_amount), 0), t.issued_by
		FROM personal_access_tokens t
		LEFT JOIN qos_events e ON e.token_id = t.id
		WHERE t.owner_subject=$1
		GROUP BY t.id
		ORDER BY t.created_at DESC`, s.Subject)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer rows.Close()
	items := []tokenRecord{}
	for rows.Next() {
		var item tokenRecord
		if err := rows.Scan(&item.ID, &item.Name, &item.Prefix, &item.CreatedAt, &item.ExpiresAt, &item.RevokedAt, &item.LastUsedAt, &item.CostAmount, &item.IssuedBy); err != nil {
			http.Error(w, "database error", 500)
			return
		}
		items = append(items, item)
	}
	writeJSON(w, 200, map[string]any{"currency": a.pricing.Currency, "tokens": items})
}

func (a *app) createToken(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	var input struct {
		Name          string `json:"name"`
		ExpiresInDays int    `json:"expires_in_days"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&input) != nil {
		http.Error(w, "invalid JSON", 400)
		return
	}
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 100 {
		http.Error(w, "name must be 1-100 characters", 400)
		return
	}
	if input.ExpiresInDays == 0 {
		input.ExpiresInDays = 90
	}
	if input.ExpiresInDays < 1 || input.ExpiresInDays > 365 {
		http.Error(w, "expires_in_days must be 1-365", 400)
		return
	}
	raw, err := randomURL(32)
	if err != nil {
		http.Error(w, "random source unavailable", 500)
		return
	}
	value := "sk-" + raw
	id, err := randomURL(16)
	if err != nil {
		http.Error(w, "random source unavailable", 500)
		return
	}
	expires := time.Now().UTC().AddDate(0, 0, input.ExpiresInDays)
	ownerName := strings.TrimSpace(s.Username)
	if ownerName == "" {
		ownerName = s.Subject
	}
	_, err = a.db.Exec(r.Context(), `INSERT INTO personal_access_tokens (id,owner_subject,owner_name,token_hash,token_prefix,name,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7)`, id, s.Subject, ownerName, a.tokenHash(value), value[:15], input.Name, expires)
	if err != nil {
		http.Error(w, "could not create token", 500)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "token": value, "name": input.Name, "expires_at": expires})
}

func (a *app) revokeToken(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/tokens/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	command, err := a.db.Exec(r.Context(), `UPDATE personal_access_tokens SET revoked_at=COALESCE(revoked_at, now()) WHERE id=$1 AND owner_subject=$2`, id, s.Subject)
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if command.RowsAffected() == 0 {
		http.NotFound(w, r)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *app) proxy(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !strings.HasPrefix(token, "sk-") || strings.TrimSpace(token) != token {
		openAIError(w, 401, "Invalid authentication credentials")
		return
	}
	id, owner, ownerName, tokenName, err := a.lookupPAT(r.Context(), token)
	if errors.Is(err, errDatabaseUnavailable) {
		http.Error(w, "database unavailable", 503)
		return
	}
	if err != nil {
		openAIError(w, 401, "Invalid authentication credentials")
		return
	}
	if a.cfg.logClientShape {
		r.Body = a.logClientShape(r, owner)
	}
	var sessionResult qos.Result
	r.Body, sessionResult = a.observeSession(r, owner)
	bearer, err := a.gatewayBearer(r.Context())
	if err != nil {
		log.Printf("gateway credential: %v", err)
		http.Error(w, "inference authorization unavailable", 503)
		return
	}
	target := strings.TrimRight(a.cfg.gatewayURL, "/") + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, r.Body)
	if err != nil {
		http.Error(w, "invalid request", 400)
		return
	}
	copyRequestHeaders(req.Header, r.Header)
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("X-User-Id", owner)
	if ownerName == "" {
		ownerName = owner
	}
	req.Header.Set("X-User-Name", ownerName)
	req.Header.Set("X-Pat-Token-Id", id)
	// llm-d flow control treats this header as the fairness tenant. It must be
	// derived after PAT validation; allowing a caller to provide it would let a
	// tenant bypass its own queue by creating arbitrary fairness identities.
	req.Header.Set("X-Llm-D-Inference-Fairness-Id", owner)
	// Selects the InferenceObjective (ADR 0008 "Stage 1B") that resolves
	// this request's priority band in llm-d's EPP. Same reasoning as
	// fairness id above: must be server-derived, never client-supplied.
	req.Header.Set("X-Llm-D-Inference-Objective", string(sessionResult.Band))
	if sessionResult.SessionID != "" {
		req.Header.Set("X-Session-Key", sessionResult.SessionID)
	}
	resp, err := a.proxyHTTP.Do(req)
	if err != nil {
		if strings.Contains(r.URL.Path, "/chat/completions") {
			a.qos.Metrics().RequestsTotal.WithLabelValues(owner, string(sessionResult.Band), "error").Inc()
		}
		http.Error(w, "inference unavailable", 502)
		return
	}
	defer resp.Body.Close()
	model := sessionResult.Model
	if model == "" {
		model = "unknown"
	}
	resp.Body = a.recordUsage(r, resp, owner, ownerName, id, tokenName, sessionResult.SessionID, model, sessionResult.Band)
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

var errDatabaseUnavailable = errors.New("database unavailable")

// lookupPAT resolves a live (not revoked, not expired) PAT and records its
// use. Shared by /v1/ and /mcp/web-search/ so revocation cuts both.
func (a *app) lookupPAT(ctx context.Context, token string) (id, owner, ownerName, tokenName string, err error) {
	err = a.db.QueryRow(ctx, `SELECT id,owner_subject,owner_name,name FROM personal_access_tokens WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at > now()`, a.tokenHash(token)).Scan(&id, &owner, &ownerName, &tokenName)
	if err != nil {
		return "", "", "", "", err
	}
	if _, err := a.db.Exec(ctx, `UPDATE personal_access_tokens SET last_used_at=now() WHERE id=$1`, id); err != nil {
		return "", "", "", "", errDatabaseUnavailable
	}
	return id, owner, ownerName, tokenName, nil
}

// recordUsage is TASK-qos-fair-share.md §4.4: for a non-streaming
// chat-completion response it reads usage.prompt_tokens/completion_tokens
// (prompt_tokens_details.cached_tokens too, though ADR 0008 "Verifying the
// prediction" found this vLLM build never populates it) and feeds them
// into the requesting user's spend for qos.Tracker.assignBand to read on
// *future* requests -- cost is only knowable after the response completes,
// so this can never affect the request that produced it. It never buffers
// a streamed (text/event-stream) response, reads at most qosUsageMaxBody
// bytes of a non-streamed one, and always returns a reader that reproduces
// resp.Body exactly, matching observeSession's approach on the request
// side: a large or unparsable response is proxied unaffected, just without
// a spend update. It also increments Stage 4's patsvc_requests_total, the
// only per-request activity counter this package emits directly instead of
// through qos.Tracker.
//
// docs/adr/0013-embeddings-api-bge-m3.md extends this to non-streaming
// /v1/embeddings responses: prompt_tokens feeds patsvc_embedding_tokens_total
// instead of qos.RecordCost, and patsvc_requests_total is recorded with a
// literal "embeddings" band, since embeddings never goes through
// observeSession's chat-only session/band assignment (band is always
// qos.BandNormal there for this path, which would be misleading here).
//
// It also writes one qos_events row per completed chat or embeddings
// response (usage.go) for the /platform usage panel -- pricing.cost, not
// qos.RecordCost's alpha/beta units, since the panel shows real currency.
// Embeddings get a row (band "embeddings", cost only) but never
// qos.RecordCost, matching the reasoning above: embeddings carry no QoS
// cost unit, only a real price.
func (a *app) recordUsage(r *http.Request, resp *http.Response, owner, ownerName, tokenID, tokenName, sessionID, model string, band qos.Band) io.ReadCloser {
	isChat := strings.Contains(r.URL.Path, "/chat/completions")
	isEmbeddings := strings.Contains(r.URL.Path, "/embeddings")
	if !isChat && !isEmbeddings {
		return resp.Body
	}
	recordBand := band
	if isEmbeddings {
		recordBand = "embeddings"
	}
	outcome := "dispatched"
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		outcome = "rejected"
	case resp.StatusCode >= 500:
		outcome = "error"
	}
	a.qos.Metrics().RequestsTotal.WithLabelValues(owner, string(recordBand), outcome).Inc()

	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		// Embeddings never stream, and a non-200 stream (rare, but the
		// gateway can emit an SSE error frame) never carries a usage
		// chunk worth waiting for.
		if !isChat || outcome != "dispatched" {
			return resp.Body
		}
		ctx := r.Context()
		return newSSEUsageReader(resp.Body, func(usageModel string, promptTokens, cachedTokens, completionTokens int64) {
			if usageModel == "" {
				usageModel = model
			}
			recordCtx, cancel := context.WithTimeout(ctx, a.cfg.qosEventTimeout)
			defer cancel()
			a.qos.RecordCost(recordCtx, owner, usageModel, promptTokens, cachedTokens, completionTokens)
			a.recordQosEvent(recordCtx, owner, ownerName, tokenID, tokenName, sessionID, usageModel, string(recordBand), promptTokens, cachedTokens, completionTokens)
		})
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, a.cfg.qosUsageMaxBody+1))
	if err != nil {
		return resp.Body
	}
	restored := io.NopCloser(io.MultiReader(bytes.NewReader(body), resp.Body))
	if int64(len(body)) > a.cfg.qosUsageMaxBody {
		return restored
	}
	var parsed struct {
		Model string `json:"model"`
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &parsed) != nil {
		return restored
	}
	usageModel := model
	if parsed.Model != "" {
		usageModel = parsed.Model
	}
	if isEmbeddings {
		if parsed.Usage.PromptTokens > 0 {
			a.qos.Metrics().EmbeddingTokensTotal.WithLabelValues(owner, usageModel).Add(float64(parsed.Usage.PromptTokens))
			ctx, cancel := context.WithTimeout(r.Context(), a.cfg.qosEventTimeout)
			defer cancel()
			a.recordQosEvent(ctx, owner, ownerName, tokenID, tokenName, "", usageModel, string(recordBand), parsed.Usage.PromptTokens, 0, 0)
		}
		return restored
	}
	if parsed.Usage.PromptTokens > 0 || parsed.Usage.CompletionTokens > 0 {
		ctx, cancel := context.WithTimeout(r.Context(), a.cfg.qosFingerprintTimeout)
		a.qos.RecordCost(ctx, owner, usageModel, parsed.Usage.PromptTokens, parsed.Usage.PromptTokensDetails.CachedTokens, parsed.Usage.CompletionTokens)
		cancel()
		eventCtx, eventCancel := context.WithTimeout(r.Context(), a.cfg.qosEventTimeout)
		defer eventCancel()
		a.recordQosEvent(eventCtx, owner, ownerName, tokenID, tokenName, sessionID, usageModel, string(recordBand), parsed.Usage.PromptTokens, parsed.Usage.PromptTokensDetails.CachedTokens, parsed.Usage.CompletionTokens)
	}
	return restored
}

// logClientShape is TASK-qos-fair-share.md Stage 0.4 diagnostic
// instrumentation: it logs which header names and top-level JSON body keys a
// real client actually sent, never a value, since the body carries the
// customer's own code. It reads at most clientShapeSniffLimit bytes of the
// body and returns a replacement reader that reproduces the original stream
// exactly, so the proxied request is unaffected either way.
const clientShapeSniffLimit = 1 << 20

func (a *app) logClientShape(r *http.Request, owner string) io.ReadCloser {
	headerNames := make([]string, 0, len(r.Header))
	for name := range r.Header {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)

	head, err := io.ReadAll(io.LimitReader(r.Body, clientShapeSniffLimit+1))
	if err != nil {
		log.Printf("qos client-shape: owner=%s read body: %v", owner, err)
		return r.Body
	}
	restored := io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))

	bodyKeys := "unavailable"
	if len(head) <= clientShapeSniffLimit {
		var fields map[string]json.RawMessage
		if json.Unmarshal(head, &fields) == nil {
			keys := make([]string, 0, len(fields))
			for k := range fields {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			bodyKeys = strings.Join(keys, ",")
		} else {
			bodyKeys = "not-a-json-object"
		}
	} else {
		bodyKeys = "body-exceeds-sniff-limit"
	}
	log.Printf("qos client-shape: owner=%s user_agent=%q headers=%s body_keys=%s",
		owner, r.Header.Get("User-Agent"), strings.Join(headerNames, ","), bodyKeys)
	return restored
}

// observeSession is TASK-qos-fair-share.md Stage 3: it computes the
// chain-hash session match for chat-completion requests, records it as
// Valkey state and Prometheus metrics, and assigns the request's priority
// band (§4.2). It never blocks, delays, or reorders the request itself --
// the queue and the admission decision are llm-d's (§2.1) -- so a slow or
// unreachable Valkey degrades the session match and band to "normal", never
// the request. It reads at most qosFingerprintMaxBody bytes and always
// returns a reader that reproduces the original body exactly, matching
// logClientShape's approach so Content-Length (copied through from the
// client's own header by copyRequestHeaders) stays correct either way.
func (a *app) observeSession(r *http.Request, owner string) (io.ReadCloser, qos.Result) {
	degraded := qos.Result{Band: qos.BandNormal}
	if !strings.Contains(r.URL.Path, "/chat/completions") {
		return r.Body, degraded
	}
	head, err := io.ReadAll(io.LimitReader(r.Body, a.cfg.qosFingerprintMaxBody+1))
	if err != nil {
		return r.Body, degraded
	}
	if int64(len(head)) > a.cfg.qosFingerprintMaxBody {
		return io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body)), degraded
	}
	// Usage panel needs a final usage chunk out of streamed chat
	// completions (main draw of TASK-*-usage-panel.md); vLLM/SGLang only
	// emit one when the request asks for it. Only touches the body when
	// stream=true and the client hasn't already set it -- see usage.go.
	if mutated, ok := ensureStreamUsage(head); ok {
		head = mutated
	}
	restored := io.NopCloser(io.MultiReader(bytes.NewReader(head), r.Body))
	ctx, cancel := context.WithTimeout(r.Context(), a.cfg.qosFingerprintTimeout)
	defer cancel()
	result := a.qos.Observe(ctx, owner, head)
	if result.Band == "" {
		// Degraded outcome (bad JSON, chain error, store error): Observe
		// never computes a band on that path, so default it here rather
		// than teach Observe's early returns about band's zero value.
		result.Band = qos.BandNormal
	}
	return restored, result
}

func (a *app) gatewayBearer(ctx context.Context) (string, error) {
	a.gateway.mu.Lock()
	defer a.gateway.mu.Unlock()
	if a.gateway.value != "" && time.Now().Add(time.Minute).Before(a.gateway.expires) {
		return a.gateway.value, nil
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {a.cfg.gatewayClientID}, "client_secret": {a.cfg.gatewaySecret}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.internalIssuer+"/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("token endpoint returned %s", resp.Status)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("token endpoint returned no access token")
	}
	a.gateway.value = out.AccessToken
	a.gateway.expires = time.Now().Add(time.Duration(out.ExpiresIn) * time.Second)
	return out.AccessToken, nil
}

func (a *app) tokenHash(token string) []byte {
	h := hmac.New(sha256.New, a.cfg.hashKey)
	h.Write([]byte(token))
	return h.Sum(nil)
}
func (a *app) requireSession(w http.ResponseWriter, r *http.Request) (session, bool) {
	s, ok := a.currentSession(r)
	if !ok {
		openAIError(w, 401, "SSO login required")
		return session{}, false
	}
	return s, true
}
func (a *app) currentSession(r *http.Request) (session, bool) {
	var s session
	if !a.readSignedCookie(r, cookieName, &s) || s.Expires < time.Now().Unix() || !hasAIUserRole(s.Roles) {
		return session{}, false
	}
	return s, true
}
func hasAIUserRole(roles []string) bool {
	for _, role := range roles {
		if role == "ai-user" || role == "ai-admin" {
			return true
		}
	}
	return false
}

func (a *app) setSignedCookie(w http.ResponseWriter, name string, value any, seconds int) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	sig := a.sign(payload)
	http.SetCookie(w, &http.Cookie{Name: name, Value: payload + "." + sig, Path: "/", MaxAge: seconds, HttpOnly: true, Secure: a.cfg.secureCookies, SameSite: http.SameSiteLaxMode})
	return nil
}
func (a *app) readSignedCookie(r *http.Request, name string, target any) bool {
	c, err := r.Cookie(name)
	if err != nil {
		return false
	}
	parts := strings.Split(c.Value, ".")
	if len(parts) != 2 || !hmac.Equal([]byte(parts[1]), []byte(a.sign(parts[0]))) {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	return err == nil && json.Unmarshal(raw, target) == nil
}
func (a *app) sign(value string) string {
	h := hmac.New(sha256.New, a.cfg.cookieKey)
	h.Write([]byte(value))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}

func (a *app) verifyJWT(ctx context.Context, raw string) (claims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return claims{}, errors.New("malformed JWT")
	}
	var head struct {
		Alg string `json:"alg"`
		KID string `json:"kid"`
	}
	if decodeJWTPart(parts[0], &head) != nil || head.Alg != "RS256" || head.KID == "" {
		return claims{}, errors.New("unsupported JWT")
	}
	var cl claims
	if decodeJWTPart(parts[1], &cl) != nil || cl.Issuer != a.cfg.issuer || cl.Subject == "" || cl.Expires <= time.Now().Unix() {
		return claims{}, errors.New("invalid JWT claims")
	}
	key, err := a.key(ctx, head.KID)
	if err != nil {
		return claims{}, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return claims{}, err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if rsa.VerifyPKCS1v15(key, crypto.SHA256, sum[:], sig) != nil {
		return claims{}, errors.New("invalid JWT signature")
	}
	return cl, nil
}
func decodeJWTPart(raw string, dst any) error {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}
func (a *app) key(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	a.keys.mu.Lock()
	defer a.keys.mu.Unlock()
	if time.Now().Before(a.keys.expires) {
		if key := a.keys.keys[kid]; key != nil {
			return key, nil
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.cfg.internalIssuer+"/protocol/openid-connect/certs", nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("JWKS returned %s", resp.Status)
	}
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, err
	}
	keys := map[string]*rsa.PublicKey{}
	for _, item := range doc.Keys {
		if item.KTY != "RSA" {
			continue
		}
		n := base64URLBigInt(item.N)
		if n.Sign() <= 0 {
			continue
		}
		eBig := base64URLBigInt(item.E)
		if !eBig.IsInt64() {
			continue
		}
		keys[item.KID] = &rsa.PublicKey{N: n, E: int(eBig.Int64())}
	}
	a.keys.keys = keys
	a.keys.expires = time.Now().Add(15 * time.Minute)
	if key := keys[kid]; key != nil {
		return key, nil
	}
	return nil, errors.New("JWT signing key not found")
}
func base64URLBigInt(raw string) *big.Int {
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return big.NewInt(0)
	}
	return new(big.Int).SetBytes(b)
}

func randomURL(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; script-src 'self'")
		next.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func openAIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"message": message, "type": "invalid_request_error"}})
}
func copyRequestHeaders(dst, src http.Header) {
	for k, values := range src {
		key := http.CanonicalHeaderKey(k)
		if key == "Authorization" || key == "Host" || key == "X-User-Id" || key == "X-User-Name" || key == "X-Pat-Token-Id" || key == "X-Llm-D-Inference-Fairness-Id" || key == "X-Llm-D-Inference-Objective" || key == "X-Session-Key" || key == "Agent-Session-Id" || isHopByHop(key) {
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}
}
func copyResponseHeaders(dst, src http.Header) {
	for k, values := range src {
		if isHopByHop(k) {
			continue
		}
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}
func isHopByHop(key string) bool {
	switch http.CanonicalHeaderKey(key) {
	case "Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade":
		return true
	}
	return false
}
