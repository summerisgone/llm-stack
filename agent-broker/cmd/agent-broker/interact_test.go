package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInteractionTurnAndPermission(t *testing.T) {
	var s *server
	var turnBody map[string]any
	release := make(chan struct{})
	adapter := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/agent/turns":
			_ = json.NewDecoder(r.Body).Decode(&turnBody)
			w.Header().Set("Content-Type", "application/x-ndjson")
			_, _ = io.WriteString(w, `{"type":"turn","turn":"t1"}`+"\n"+`{"type":"permission","turn":"t1","request":"1"}`+"\n")
			w.(http.Flusher).Flush()
			<-release
			_, _ = io.WriteString(w, `{"type":"text","text":"hi"}`+"\n"+`{"type":"done","stop":"end_turn"}`+"\n")
		case "/v1/agent/permissions":
			var p map[string]any
			_ = json.NewDecoder(r.Body).Decode(&p)
			if p["turn"] != "t1" || p["request"] != "1" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}))
	defer adapter.Close()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	s = &server{
		auth:     NewVerifier(testIssuer, jwksServer(t, key, "k1").URL, []string{"ai-user"}),
		runtimes: []Runtime{{Name: "dsh", ModelID: "dsh-agent"}, {Name: "pi", ModelID: "pi-agent"}},
		client:   http.DefaultClient,
		pipe:     map[string]bool{"dsh": true},
	}
	s.backend = webBackend{newFake(), adapter.URL}
	s.slots = newTestSlots(s.backend, &clock{})
	broker := httptest.NewServer(s.routes())
	defer broker.Close()
	token := signToken(t, key, "k1", claims("ai-user"))
	post := func(path, authz string, body any) *http.Response {
		raw, _ := json.Marshal(body)
		req, _ := http.NewRequest(http.MethodPost, broker.URL+path, bytes.NewReader(raw))
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	turn := map[string]any{"model": "dsh-agent", "chat_id": "c1", "users": []string{"hello"}}

	if res := post("/v1/agent/turns", "", turn); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.StatusCode)
	}
	if res := post("/v1/agent/turns", token, map[string]any{"model": "pi-agent", "users": []string{"x"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("runtime without the pipe: %d", res.StatusCode)
	}
	// No running agent yet: nothing to answer.
	if res := post("/v1/agent/permissions", token, map[string]any{"model": "dsh-agent", "turn": "t1", "request": "1", "allow": true}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("permission without agent: %d", res.StatusCode)
	}

	res := post("/v1/agent/turns", token, turn)
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "application/x-ndjson" {
		t.Fatalf("turn: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}
	rd := bufio.NewReader(res.Body)
	var types []string
	next := func() map[string]any {
		line, err := rd.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read event: %v", err)
		}
		var e map[string]any
		_ = json.Unmarshal(line, &e)
		types = append(types, e["type"].(string))
		return e
	}
	next()
	if e := next(); e["type"] != "permission" {
		t.Fatalf("event %v", e)
	}
	if st := s.slots.Stats(); st.Busy != 1 {
		t.Fatalf("a pending permission must hold the slot: %+v", st)
	}
	if r := post("/v1/agent/permissions", token, map[string]any{"model": "dsh-agent", "turn": "t1", "request": "2", "allow": true}); r.StatusCode != http.StatusNotFound {
		t.Fatalf("stale request: %d", r.StatusCode)
	}
	if r := post("/v1/agent/permissions", token, map[string]any{"model": "dsh-agent", "turn": "t1", "request": "1", "allow": true}); r.StatusCode != http.StatusOK {
		t.Fatalf("permission: %d", r.StatusCode)
	}
	close(release)
	next()
	next()
	// EOF comes after the handler returned, so after Release.
	_, _ = io.ReadAll(rd)
	res.Body.Close()
	if strings.Join(types, ",") != "turn,permission,text,done" {
		t.Fatalf("events %v", types)
	}
	if turnBody["chat_id"] != "c1" || turnBody["model"] != nil {
		t.Fatalf("adapter got %v", turnBody)
	}
	if st := s.slots.Stats(); st.Busy != 0 {
		t.Fatalf("slot not released: %+v", st)
	}
}
