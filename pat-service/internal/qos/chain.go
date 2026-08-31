// Package qos implements TASK-qos-fair-share.md's per-user fair-share
// scheduler for pat-service. Stage 3 step 1 only: chain-hash session
// identification, observed and recorded, never gating admission.
package qos

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// chatRequest pulls out only the fields the chain formula needs
// (TASK-qos-fair-share.md §3.1); everything else in the body is irrelevant
// to session identity and is never inspected here.
type chatRequest struct {
	Model    string            `json:"model"`
	Tools    json.RawMessage   `json:"tools"`
	Messages []json.RawMessage `json:"messages"`
}

// parseChatRequest reports ok=false for anything that is not a chat
// completion with at least one message -- the chain formula has no meaning
// for /v1/completions, /v1/models, embeddings, etc.
func parseChatRequest(body []byte) (req chatRequest, ok bool) {
	if err := json.Unmarshal(body, &req); err != nil {
		return chatRequest{}, false
	}
	if len(req.Messages) == 0 {
		return chatRequest{}, false
	}
	if req.Tools == nil {
		req.Tools = json.RawMessage("null")
	}
	return req, true
}

// canonicalize is TASK-qos-fair-share.md §3.1's "canonical(...)": a
// deterministic serialization with sorted keys and no insignificant
// whitespace. encoding/json.Marshal already sorts map[string]any keys and
// emits no extra whitespace, so decoding to `any` and re-encoding is
// sufficient -- no bespoke serializer needed.
func canonicalize(raw json.RawMessage) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// chain computes c_0..c_n per §3.1:
//
//	c_0 = H(model, canonical(tools), canonical(messages[0]))
//	c_i = H(c_{i-1}, canonical(messages[i]))
//
// A NUL byte separates fields fed into each hash so that concatenation
// cannot alias across a field boundary; H is SHA-256, hex-encoded for use as
// a Valkey key component. Returned links are ordered oldest to newest.
func chain(req chatRequest) ([]string, error) {
	canonTools, err := canonicalize(req.Tools)
	if err != nil {
		return nil, err
	}
	links := make([]string, len(req.Messages))
	var prev []byte
	for i, msg := range req.Messages {
		canonMsg, err := canonicalize(msg)
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		if i == 0 {
			h.Write([]byte(req.Model))
			h.Write([]byte{0})
			h.Write(canonTools)
			h.Write([]byte{0})
			h.Write(canonMsg)
		} else {
			h.Write(prev)
			h.Write([]byte{0})
			h.Write(canonMsg)
		}
		sum := h.Sum(nil)
		links[i] = hex.EncodeToString(sum)
		prev = sum
	}
	return links, nil
}
