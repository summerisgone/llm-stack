package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// Dashboard issuance of the agents' inference key (docs/adr/0014 section 8,
// user-initiated instead of broker-initiated): mint a `hermes-agent` PAT,
// revoke the previous one, write it as INFERENCE_KEY into agent-cred-<id>,
// which all of the user's agents (Hermes, pi, opencode) read, and delete
// their running pods so the next start reads the new key. Replaces
// scripts/agent-inference-key.

const (
	agentTokenName = "agent"
	agentIssuedBy  = "agents"
	saDir          = "/var/run/secrets/kubernetes.io/serviceaccount/"
)

// agentKube is a minimal in-cluster client for the two calls this needs in
// the agents namespace. nil when pat-service runs outside a cluster.
type agentKube struct {
	base      string
	ns        string
	tokenFile string
	client    *http.Client
}

func newAgentKube(ns string) *agentKube {
	ca, err := os.ReadFile(saDir + "ca.crt")
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	if err != nil || host == "" {
		return nil
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	return &agentKube{
		base:      "https://" + strings.Trim(host, "[]") + ":" + os.Getenv("KUBERNETES_SERVICE_PORT"),
		ns:        ns,
		tokenFile: saDir + "token",
		client:    &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}
}

// do returns the response status and decodes a 2xx body into out if set;
// 404 is not an error so callers can upsert and ignore missing pods.
func (k *agentKube) do(ctx context.Context, method, resource, name, contentType string, body, out any) (int, error) {
	// Projected ServiceAccount tokens rotate; read the current one each call.
	token, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return 0, err
	}
	path := k.base + "/api/v1/namespaces/" + k.ns + "/" + resource
	if name != "" {
		path += "/" + name
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return 0, err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, path, rd)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode != http.StatusNotFound {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return resp.StatusCode, fmt.Errorf("%s %s/%s: %d %s", method, resource, name, resp.StatusCode, msg)
	}
	if out != nil && resp.StatusCode < 300 {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return resp.StatusCode, err
		}
	}
	return resp.StatusCode, nil
}

// agentProfileID matches agent-broker's profileID: first 16 hex chars of
// sha256(sub).
func agentProfileID(sub string) string {
	sum := sha256.Sum256([]byte(sub))
	return hex.EncodeToString(sum[:])[:16]
}

// storeInferenceKey upserts INFERENCE_KEY into agent-cred-<id>, keeping the
// broker's API_SERVER_KEY, then deletes the user's agent pods. Reports
// whether any was running.
func (k *agentKube) storeInferenceKey(ctx context.Context, id, key string, expires time.Time) (bool, error) {
	secret := "agent-cred-" + id
	meta := map[string]any{"annotations": map[string]string{"agents.llm-stack/inference-key-expires-at": expires.Format(time.RFC3339)}}
	status, err := k.do(ctx, http.MethodGet, "secrets", secret, "", nil, nil)
	if err != nil {
		return false, err
	}
	if status == http.StatusNotFound {
		// Same labels agent-broker puts on its runtime objects.
		meta["name"] = secret
		meta["labels"] = map[string]string{"agents.llm-stack/user-id": id, "app.kubernetes.io/managed-by": "agent-broker", "app.kubernetes.io/name": "agent"}
		_, err = k.do(ctx, http.MethodPost, "secrets", "", "application/json", map[string]any{
			"apiVersion": "v1", "kind": "Secret", "type": "Opaque", "metadata": meta, "stringData": map[string]string{"INFERENCE_KEY": key}}, nil)
	} else {
		_, err = k.do(ctx, http.MethodPatch, "secrets", secret, "application/merge-patch+json", map[string]any{
			"metadata": meta, "stringData": map[string]string{"INFERENCE_KEY": key}}, nil)
	}
	if err != nil {
		return false, err
	}
	// Agent pods only: the broker labels its PVCs and Secrets with the user
	// id too, but those are not pods.
	var deleted struct {
		Items []json.RawMessage `json:"items"`
	}
	sel := url.QueryEscape("agents.llm-stack/user-id=" + id + ",app.kubernetes.io/managed-by=agent-broker")
	_, err = k.do(ctx, http.MethodDelete, "pods?labelSelector="+sel, "", "", nil, &deleted)
	return err == nil && len(deleted.Items) > 0, err
}

func (a *app) issueAgentToken(w http.ResponseWriter, r *http.Request) {
	s, ok := a.requireSession(w, r)
	if !ok {
		return
	}
	if a.agents == nil {
		http.Error(w, "agents are not available on this deployment", http.StatusServiceUnavailable)
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
	expires := time.Now().UTC().AddDate(0, 0, a.cfg.agentTTLDays)
	ownerName := strings.TrimSpace(s.Username)
	if ownerName == "" {
		ownerName = s.Subject
	}
	tx, err := a.db.Begin(r.Context())
	if err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	defer tx.Rollback(r.Context())
	if _, err := tx.Exec(r.Context(), `UPDATE personal_access_tokens SET revoked_at=now() WHERE owner_subject=$1 AND issued_by=$2 AND revoked_at IS NULL`, s.Subject, agentIssuedBy); err != nil {
		http.Error(w, "database unavailable", 503)
		return
	}
	if _, err := tx.Exec(r.Context(), `INSERT INTO personal_access_tokens (id,owner_subject,owner_name,token_hash,token_prefix,name,expires_at,issued_by) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`, id, s.Subject, ownerName, a.tokenHash(value), value[:15], agentTokenName, expires, agentIssuedBy); err != nil {
		http.Error(w, "could not create token", 500)
		return
	}
	// The Secret is written before commit: if it fails, the previous key
	// stays both valid and in place.
	profile := agentProfileID(s.Subject)
	restarted, err := a.agents.storeInferenceKey(r.Context(), profile, value, expires)
	if err != nil {
		log.Printf("agent key for profile %s: %v", profile, err)
		http.Error(w, "could not deliver key to the agents", 502)
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		http.Error(w, "could not create token", 500)
		return
	}
	writeJSON(w, 201, map[string]any{"id": id, "name": agentTokenName, "expires_at": expires, "profile_id": profile, "agent_restarted": restarted})
}
