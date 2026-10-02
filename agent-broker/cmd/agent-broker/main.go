// agent-broker is Open WebUI's only path to per-user agents (docs/adr/0009,
// docs/adr/0014 section 6): Hermes, and pi / opencode when their images are
// configured, each listed as a model. It validates the user's Keycloak
// token, answers `/skills` commands itself, starts the user's agent for the
// requested model in one of K shared slots and streams the chat completion
// through to it.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type server struct {
	runtimes []Runtime
	auth     *Verifier
	slots    *Slots
	backend  Backend
	catalog  *Catalog
	client   *http.Client
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key, def string) time.Duration {
	d, err := time.ParseDuration(env(key, def))
	if err != nil {
		slog.Error("bad duration", "key", key, "err", err)
		os.Exit(1)
	}
	return d
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		slog.Error("missing required env", "key", key)
		os.Exit(1)
	}
	return v
}

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))

	catalog, err := LoadCatalog(env("CATALOG_INDEX", "/catalog/catalog.json"))
	if err != nil {
		slog.Error("load catalog", "err", err)
		os.Exit(1)
	}
	catalogImage := mustEnv("AGENT_CATALOG_IMAGE")
	k, _ := strconv.Atoi(env("AGENT_SLOTS", "3"))
	runtimes := []Runtime{{Name: "hermes", ModelID: "hermes-agent", Image: mustEnv("HERMES_IMAGE"),
		Title:       "Hermes agent",
		Description: "Your personal Hermes agent with the curated skill catalog. Type /skills to see and switch skills."}}
	// pi and opencode run behind agent-adapter; each is offered only when its
	// image is configured (agent-images ConfigMap).
	if img := os.Getenv("PI_IMAGE"); img != "" {
		runtimes = append(runtimes, Runtime{Name: "pi", ModelID: "pi-agent", Image: img,
			Title:       "Pi agent",
			Description: "Your personal pi coding agent with the curated skill catalog. Type /skills to see and switch skills."})
	}
	if img := os.Getenv("OPENCODE_IMAGE"); img != "" {
		runtimes = append(runtimes, Runtime{Name: "opencode", ModelID: "opencode-agent", Image: img,
			Title:       "OpenCode agent",
			Description: "Your personal OpenCode agent with the curated skill catalog. Type /skills to see and switch skills."})
	}
	// DeepSeek Harness: chats over ACP and, with DSH_PUBLIC_ORIGIN, the
	// user's own dsh web UI through the web proxy (web.go, docs/adr/0020).
	dshOrigin := os.Getenv("DSH_PUBLIC_ORIGIN")
	var dshAuthority string
	if img := os.Getenv("DSH_IMAGE"); img != "" {
		runtimes = append(runtimes, Runtime{Name: "dsh", ModelID: "dsh-agent", Image: img,
			Title:       "DeepSeek agent",
			Description: "Your personal DeepSeek Harness agent with the curated skill catalog. Type /skills to see and switch skills."})
		if dshOrigin != "" {
			if dshAuthority, err = publicAuthority(dshOrigin); err != nil {
				slog.Error("dsh web", "err", err)
				os.Exit(1)
			}
		}
	}
	stopGrace, _ := strconv.ParseInt(env("AGENT_STOP_GRACE_SECONDS", "30"), 10, 64)

	var backend Backend
	switch b := env("AGENT_BACKEND", "pods"); b {
	case "pods":
		kc, err := newInClusterKube(mustEnv("POD_NAMESPACE"))
		if err != nil {
			slog.Error("kubernetes client", "err", err)
			os.Exit(1)
		}
		backend = NewPodsBackend(kc, PodsConfig{
			Runtimes:     runtimes,
			RuntimeClass: os.Getenv("AGENT_RUNTIME_CLASS"),
			StorageClass: os.Getenv("PROFILE_STORAGE_CLASS"),
			ProfileSize:  env("PROFILE_SIZE", "2Gi"),
			CPURequest:   env("AGENT_CPU_REQUEST", "100m"),
			CPULimit:     env("AGENT_CPU_LIMIT", "1"),
			MemRequest:   env("AGENT_MEMORY_REQUEST", "384Mi"),
			MemLimit:     env("AGENT_MEMORY_LIMIT", "1Gi"),
			WorkSize:     env("AGENT_WORK_SIZE", "2Gi"),
			StartTimeout: envDuration("AGENT_START_TIMEOUT", "180s"),
			StopGrace:    stopGrace,
			WebSearch:    os.Getenv("WEB_SEARCH_ENABLED") == "true",
			Repowise:     os.Getenv("REPOWISE_ENABLED") == "true",
			DshMemLimit:  env("DSH_MEMORY_LIMIT", "2Gi"),
			DshWebHost:   dshAuthority,
		})
	default:
		slog.Error("unsupported AGENT_BACKEND", "backend", b)
		os.Exit(1)
	}

	slots := NewSlots(SlotsConfig{
		K:                k,
		IdleTimeout:      envDuration("IDLE_TIMEOUT", "15m"),
		IdleShutdown:     envDuration("IDLE_SHUTDOWN", "30m"),
		MaxAgentLifetime: envDuration("MAX_AGENT_LIFETIME", "8h"),
		SlotWaitTimeout:  envDuration("SLOT_WAIT_TIMEOUT", "60s"),
		CatalogImage:     func() string { return catalogImage },
	}, backend)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := slots.Recover(ctx); err != nil {
		slog.Error("recover agents", "err", err)
		os.Exit(1)
	}
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				slots.Reap(ctx)
			}
		}
	}()

	s := &server{
		runtimes: runtimes,
		auth: NewVerifier(mustEnv("OIDC_ISSUER"), mustEnv("OIDC_JWKS_URL"),
			strings.Split(env("ALLOWED_ROLES", "ai-user,ai-admin"), ",")),
		slots:   slots,
		backend: backend,
		catalog: catalog,
		// No overall timeout: agent turns run for minutes. The request
		// context cancels the upstream call when the client goes away.
		client: &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: 0,
			DialContext: (&net.Dialer{Timeout: 5 * time.Second}).DialContext}},
	}
	srv := &http.Server{Addr: env("LISTEN_ADDR", ":8080"), Handler: s.routes(),
		ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	if dshAuthority != "" {
		web := &webProxy{s: s, authority: dshAuthority, port: DshWebPort, client: s.client}
		wsrv := &http.Server{Addr: env("WEB_LISTEN_ADDR", ":8081"), Handler: web.routes(),
			ReadHeaderTimeout: 10 * time.Second}
		go func() {
			<-ctx.Done()
			_ = wsrv.Close()
		}()
		go func() {
			slog.Info("dsh web proxy listening", "addr", wsrv.Addr, "host", dshAuthority)
			if err := wsrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("serve dsh web", "err", err)
				os.Exit(1)
			}
		}()
	}
	slog.Info("agent-broker listening", "addr", srv.Addr, "slots", k, "catalog", catalog.Version)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("serve", "err", err)
		os.Exit(1)
	}
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, s.slots.Stats())
	})
	mux.HandleFunc("GET /v1/models", s.withUser(s.models))
	mux.HandleFunc("POST /v1/chat/completions", s.withUser(s.chat))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// openAIError is the error shape OpenAI clients (Open WebUI) render.
func openAIError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"type": typ, "message": msg}})
}

func (s *server) withUser(h func(http.ResponseWriter, *http.Request, User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := s.auth.Verify(r.Context(), r.Header.Get("Authorization"))
		switch {
		case errors.Is(err, errForbidden):
			openAIError(w, http.StatusForbidden, "forbidden", "the ai-user or ai-admin role is required")
			return
		case errors.Is(err, errUnauthenticated):
			openAIError(w, http.StatusUnauthorized, "unauthorized", "a valid Keycloak access token is required")
			return
		case err != nil:
			slog.Error("token verification", "err", err)
			openAIError(w, http.StatusServiceUnavailable, "unavailable", "identity provider unreachable")
			return
		}
		h(w, r, u)
	}
}

func (s *server) models(w http.ResponseWriter, _ *http.Request, _ User) {
	data := []map[string]any{}
	for _, rt := range s.runtimes {
		data = append(data, map[string]any{
			"id": rt.ModelID, "object": "model", "owned_by": "agent-broker",
			"name": rt.Title + " (personal)", "description": rt.Description,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (s *server) runtime(model string) (Runtime, bool) {
	for _, rt := range s.runtimes {
		if rt.ModelID == model {
			return rt, true
		}
	}
	return Runtime{}, false
}

type chatRequest struct {
	Model    string `json:"model"`
	Stream   bool   `json:"stream"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

// lastUserText returns the text of the last user message (string content or
// the text parts of a multimodal array).
func (c *chatRequest) lastUserText() string {
	for i := len(c.Messages) - 1; i >= 0; i-- {
		m := c.Messages[i]
		if m.Role != "user" {
			continue
		}
		var str string
		if json.Unmarshal(m.Content, &str) == nil {
			return str
		}
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(m.Content, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Type == "text" {
					b.WriteString(p.Text)
				}
			}
			return b.String()
		}
		return ""
	}
	return ""
}

func (s *server) chat(w http.ResponseWriter, r *http.Request, u User) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "cannot read body")
		return
	}
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "body is not a chat completion request")
		return
	}
	rt, ok := s.runtime(req.Model)
	if !ok {
		openAIError(w, http.StatusNotFound, "model_not_found", "unknown model "+strconv.Quote(req.Model))
		return
	}
	ref := AgentRef{Runtime: rt.Name, ID: u.ID}
	ctx := r.Context()

	created, err := s.backend.EnsureProfile(ctx, u, ref)
	if err != nil {
		slog.Error("ensure profile", "agent", ref.String(), "err", err)
		openAIError(w, http.StatusInternalServerError, "server_error", "cannot create the agent profile")
		return
	}
	if verb, args, ok := parseSkillsCommand(req.lastUserText()); ok {
		s.reply(w, req.Stream, rt.ModelID, s.skillsCommand(ctx, ref, verb, args))
		return
	}
	if created {
		slog.Info("profile created", "agent", ref.String(), "username", u.Name)
		s.reply(w, req.Stream, rt.ModelID, onboardingText(s.catalog, rt.Title))
		return
	}

	var sw *sseWriter
	if req.Stream {
		sw = newSSE(w, rt.ModelID)
	}
	// Second attempt only after the agent could not be dialled: its pod was
	// removed outside the broker and the next reconcile has not seen it yet.
	for attempt := 0; ; attempt++ {
		if !s.chatOnce(ctx, w, r, u, ref, body, req.Stream, sw) || attempt > 0 {
			return
		}
		s.slots.reconcile(ctx)
	}
}

// chatOnce serves one turn through the user's agent. It returns true, having
// written nothing to the agent's reply, when the agent was unreachable.
func (s *server) chatOnce(ctx context.Context, w http.ResponseWriter, r *http.Request, u User, ref AgentRef, body []byte, stream bool, sw *sseWriter) bool {
	agent, notice, err := s.slots.Acquire(ctx, u, ref, func() {
		if sw != nil {
			sw.text("All agent slots are busy; your request is queued...\n\n")
		}
	})
	if err != nil {
		status, msg := http.StatusBadGateway, "the agent could not be started: "+err.Error()
		if errors.Is(err, errNoSlot) {
			status, msg = http.StatusServiceUnavailable, "all agent slots are busy, try again in a minute"
			w.Header().Set("Retry-After", "30")
		}
		if sw != nil && sw.started {
			sw.text(msg)
			sw.done()
			return false
		}
		openAIError(w, status, "agent_unavailable", msg)
		return false
	}
	defer s.slots.Release(ref)

	prefix := ""
	if len(notice) > 0 {
		prefix = newSkillsNotice(notice)
	}
	return s.proxy(w, r.WithContext(ctx), agent, ref.Runtime, body, stream, sw, prefix)
}

func (s *server) skillsCommand(ctx context.Context, ref AgentRef, verb string, args []string) string {
	report, err := s.backend.Profile(ctx, ref)
	if err != nil {
		return "Cannot read your skill selection right now: " + err.Error()
	}
	sel := Selection{}
	if report != nil {
		sel = report.Sel
	}
	if p := s.slots.Pending(ref); p != nil {
		sel = *p
	}
	switch verb {
	case "list":
		var marked []string
		if report != nil {
			marked = report.New
		}
		return s.catalog.List(sel, marked) + "\n" + skillsHelp
	case "on", "off":
		if len(args) == 0 {
			return skillsHelp
		}
		next, refused := s.catalog.Apply(sel, verb == "on", args)
		msg := ""
		if len(refused) > 0 {
			msg = "Not changed: " + strings.Join(refused, ", ") + ".\n\n"
		}
		if len(refused) < len(args) {
			s.slots.SetPending(ref, next)
			msg += "Saved. Your agent restarts with the new selection before your next message.\n\n"
		}
		return msg + s.catalog.List(next, nil)
	case "reset":
		s.slots.SetPending(ref, Selection{})
		return "Catalog defaults restored; applied before your next message.\n\n" + s.catalog.List(Selection{}, nil)
	default:
		return skillsHelp
	}
}

// reply answers a request directly from the broker, in the shape the client
// asked for.
func (s *server) reply(w http.ResponseWriter, stream bool, model, text string) {
	if stream {
		sw := newSSE(w, model)
		sw.text(text)
		sw.done()
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": fmt.Sprintf("chatcmpl-broker-%d", time.Now().UnixNano()), "object": "chat.completion",
		"created": time.Now().Unix(), "model": model,
		"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
			"message": map[string]string{"role": "assistant", "content": text}}},
	})
}

func (s *server) proxy(w http.ResponseWriter, r *http.Request, a *Agent, model string, body []byte, stream bool, sw *sseWriter, prefix string) (unreachable bool) {
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, a.Endpoint+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		openAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return false
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Authorization", "Bearer "+a.APIKey)
	// agent-adapter keeps one pi / opencode session per Open WebUI chat.
	if id := r.Header.Get("X-OpenWebUI-Chat-Id"); id != "" {
		up.Header.Set("X-OpenWebUI-Chat-Id", id)
	}
	resp, err := s.client.Do(up)
	if err != nil {
		slog.Error("agent request", "agent", a.Ref.String(), "err", err)
		if opErr := (*net.OpError)(nil); errors.As(err, &opErr) && opErr.Op == "dial" {
			return true
		}
		if sw != nil && sw.started {
			sw.text("The agent did not answer: " + err.Error())
			sw.done()
			return false
		}
		openAIError(w, http.StatusBadGateway, "agent_unavailable", "the agent did not answer")
		return false
	}
	defer resp.Body.Close()

	if !stream || resp.StatusCode != http.StatusOK || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw, _ := io.ReadAll(resp.Body)
		if prefix != "" && resp.StatusCode == http.StatusOK {
			raw = prefixCompletion(raw, prefix)
		}
		if sw != nil && sw.started {
			// Status text already went out as SSE; finish in the same shape.
			sw.text(completionText(raw))
			sw.done()
			return false
		}
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(raw)
		return false
	}
	if sw == nil {
		sw = newSSE(w, model)
	}
	if prefix != "" {
		sw.text(prefix)
	}
	sw.begin()
	buf := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			sw.flush()
		}
		if err != nil {
			return false
		}
	}
}

func prefixCompletion(raw []byte, prefix string) []byte {
	var c map[string]any
	if json.Unmarshal(raw, &c) != nil {
		return raw
	}
	choices, _ := c["choices"].([]any)
	if len(choices) == 0 {
		return raw
	}
	ch, _ := choices[0].(map[string]any)
	msg, _ := ch["message"].(map[string]any)
	if msg == nil {
		return raw
	}
	content, _ := msg["content"].(string)
	msg["content"] = prefix + content
	out, err := json.Marshal(c)
	if err != nil {
		return raw
	}
	return out
}

func completionText(raw []byte) string {
	var c struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &c) == nil {
		if len(c.Choices) > 0 {
			return c.Choices[0].Message.Content
		}
		if c.Error != nil {
			return "Agent error: " + c.Error.Message
		}
	}
	return string(raw)
}

// sseWriter emits OpenAI chat.completion.chunk events for text the broker
// itself contributes to a stream (status lines, notices, command answers).
type sseWriter struct {
	w       http.ResponseWriter
	f       http.Flusher
	started bool
	id      string
	model   string
}

func newSSE(w http.ResponseWriter, model string) *sseWriter {
	f, _ := w.(http.Flusher)
	return &sseWriter{w: w, f: f, id: fmt.Sprintf("chatcmpl-broker-%d", time.Now().UnixNano()), model: model}
}

func (s *sseWriter) begin() {
	if s.started {
		return
	}
	s.started = true
	h := s.w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	s.w.WriteHeader(http.StatusOK)
}

func (s *sseWriter) flush() {
	if s.f != nil {
		s.f.Flush()
	}
}

func (s *sseWriter) text(t string) {
	s.begin()
	chunk := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]string{"role": "assistant", "content": t}}},
	}
	raw, _ := json.Marshal(chunk)
	fmt.Fprintf(s.w, "data: %s\n\n", raw)
	s.flush()
}

func (s *sseWriter) done() {
	s.begin()
	chunk := map[string]any{
		"id": s.id, "object": "chat.completion.chunk", "created": time.Now().Unix(), "model": s.model,
		"choices": []map[string]any{{"index": 0, "delta": map[string]string{}, "finish_reason": "stop"}},
	}
	raw, _ := json.Marshal(chunk)
	fmt.Fprintf(s.w, "data: %s\n\ndata: [DONE]\n\n", raw)
	s.flush()
}
