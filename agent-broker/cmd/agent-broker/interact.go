package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
)

// The interaction protocol behind the Open WebUI Pipe (docs/adr/0021). It is
// the broker's own NDJSON protocol, not ACP: the user's agent-adapter speaks
// ACP to the agent on stdio and the same protocol to the broker.
//
//	POST /v1/agent/turns {model, chat_id, users: [user texts, last is the prompt]}
//	  -> application/x-ndjson, one event per line: status, turn, notice, text,
//	     reasoning, tool, permission, then done or error. The client closing
//	     the response cancels the turn. The turn holds a slot until it ends.
//	POST /v1/agent/permissions {model, turn, request, allow}
//	  -> forwarded to the user's running agent; 404 if not pending there.
//
// Identity comes only from the verified token; a turn and its permission
// replies reach only the token subject's own agent. Runtimes not listed in
// AGENT_PIPE_RUNTIMES answer 404.

type turnRequest struct {
	Model  string   `json:"model"`
	ChatID string   `json:"chat_id"`
	Users  []string `json:"users"`
}

// ndjson writes broker-side interaction events.
type ndjson struct {
	w       http.ResponseWriter
	started bool
}

func (n *ndjson) event(e map[string]any) {
	raw, _ := json.Marshal(e)
	n.line(append(raw, '\n'))
}

func (n *ndjson) line(raw []byte) {
	if !n.started {
		n.started = true
		h := n.w.Header()
		h.Set("Content-Type", "application/x-ndjson")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		n.w.WriteHeader(http.StatusOK)
	}
	_, _ = n.w.Write(raw)
	if f, ok := n.w.(http.Flusher); ok {
		f.Flush()
	}
}

func (n *ndjson) text(t string) { n.event(map[string]any{"type": "text", "text": t}) }
func (n *ndjson) fail(msg string) {
	n.event(map[string]any{"type": "error", "message": msg})
}
func (n *ndjson) done() { n.event(map[string]any{"type": "done", "stop": "end_turn"}) }

func (s *server) pipeRuntime(w http.ResponseWriter, model string) (Runtime, bool) {
	rt, ok := s.runtime(model)
	if !ok || !s.pipe[rt.Name] {
		openAIError(w, http.StatusNotFound, "model_not_found", "no interactive agent "+strconv.Quote(model))
		return Runtime{}, false
	}
	return rt, true
}

func (s *server) turn(w http.ResponseWriter, r *http.Request, u User) {
	var req turnRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<20)).Decode(&req); err != nil {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "body is not a turn request")
		return
	}
	if len(req.Users) == 0 {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "no user message")
		return
	}
	rt, ok := s.pipeRuntime(w, req.Model)
	if !ok {
		return
	}
	ref := AgentRef{Runtime: rt.Name, ID: u.ID}
	ctx := r.Context()
	out := &ndjson{w: w}

	created, err := s.backend.EnsureProfile(ctx, u, ref)
	if err != nil {
		slog.Error("ensure profile", "agent", ref.String(), "err", err)
		openAIError(w, http.StatusInternalServerError, "server_error", "cannot create the agent profile")
		return
	}
	if verb, args, ok := parseSkillsCommand(req.Users[len(req.Users)-1]); ok {
		out.text(s.skillsCommand(ctx, ref, verb, args))
		out.done()
		return
	}
	if created {
		slog.Info("profile created", "agent", ref.String(), "username", u.Name)
		out.text(onboardingText(s.catalog, rt.Title))
		out.done()
		return
	}
	body, _ := json.Marshal(map[string]any{"chat_id": req.ChatID, "users": req.Users})
	for attempt := 0; ; attempt++ {
		if !s.turnOnce(ctx, out, u, ref, body) || attempt > 0 {
			return
		}
		s.slots.reconcile(ctx)
	}
}

// turnOnce runs one turn on the user's agent. It returns true, having
// relayed nothing from the agent, when the agent was unreachable.
func (s *server) turnOnce(ctx context.Context, out *ndjson, u User, ref AgentRef, body []byte) bool {
	agent, notice, err := s.slots.Acquire(ctx, u, ref, func() {
		out.event(map[string]any{"type": "status", "text": "All agent slots are busy; your request is queued..."})
	})
	if err != nil {
		msg := "the agent could not be started: " + err.Error()
		if errors.Is(err, errNoSlot) {
			msg = "all agent slots are busy, try again in a minute"
		}
		out.fail(msg)
		return false
	}
	defer s.slots.Release(ref)

	up, err := http.NewRequestWithContext(ctx, http.MethodPost, agent.Endpoint+"/v1/agent/turns", bytes.NewReader(body))
	if err != nil {
		out.fail(err.Error())
		return false
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Authorization", "Bearer "+agent.APIKey)
	resp, err := s.client.Do(up)
	if err != nil {
		slog.Error("agent turn", "agent", ref.String(), "err", err)
		if opErr := (*net.OpError)(nil); errors.As(err, &opErr) && opErr.Op == "dial" {
			return true
		}
		out.fail("the agent did not answer: " + err.Error())
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		out.fail("agent error: " + completionText(raw))
		return false
	}
	if len(notice) > 0 {
		out.event(map[string]any{"type": "notice", "text": newSkillsNotice(notice)})
	}
	// Lines are relayed whole: a client never sees half an event.
	rd := bufio.NewReader(resp.Body)
	for {
		line, err := rd.ReadBytes('\n')
		if err == nil {
			out.line(line)
			continue
		}
		if !errors.Is(err, io.EOF) && ctx.Err() == nil {
			out.fail("the agent connection broke: the turn's outcome is unknown")
		}
		return false
	}
}

func (s *server) permission(w http.ResponseWriter, r *http.Request, u User) {
	var req struct {
		Model   string `json:"model"`
		Turn    string `json:"turn"`
		Request string `json:"request"`
		Allow   bool   `json:"allow"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || req.Turn == "" || req.Request == "" {
		openAIError(w, http.StatusBadRequest, "invalid_request_error", "body is not a permission reply")
		return
	}
	rt, ok := s.pipeRuntime(w, req.Model)
	if !ok {
		return
	}
	agent := s.slots.Running(AgentRef{Runtime: rt.Name, ID: u.ID})
	if agent == nil {
		openAIError(w, http.StatusNotFound, "not_found", "no such pending permission request")
		return
	}
	body, _ := json.Marshal(map[string]any{"turn": req.Turn, "request": req.Request, "allow": req.Allow})
	up, err := http.NewRequestWithContext(r.Context(), http.MethodPost, agent.Endpoint+"/v1/agent/permissions", bytes.NewReader(body))
	if err != nil {
		openAIError(w, http.StatusInternalServerError, "server_error", err.Error())
		return
	}
	up.Header.Set("Content-Type", "application/json")
	up.Header.Set("Authorization", "Bearer "+agent.APIKey)
	resp, err := s.client.Do(up)
	if err != nil {
		openAIError(w, http.StatusBadGateway, "agent_unavailable", "the agent did not answer")
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 1<<20))
}
