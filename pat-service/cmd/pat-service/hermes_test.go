package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHermesProfileIDMatchesBroker(t *testing.T) {
	// hermes-broker's profileID for the same sub (first 16 hex of sha256).
	if got := hermesProfileID("alice"); got != "2bd806c97f0e00af" {
		t.Fatalf("hermesProfileID = %q", got)
	}
}

func fakeKube(t *testing.T, secretExists, podExists bool) (*hermesKube, *[]string, *map[string]any) {
	t.Helper()
	var calls []string
	var lastBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sa-token" {
			t.Errorf("missing SA bearer")
		}
		calls = append(calls, r.Method+" "+r.URL.Path)
		if r.Method == http.MethodPost || r.Method == http.MethodPatch {
			_ = json.NewDecoder(r.Body).Decode(&lastBody)
		}
		switch {
		case r.Method == http.MethodGet && !secretExists, r.Method == http.MethodDelete && !podExists:
			w.WriteHeader(http.StatusNotFound)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		}
	}))
	t.Cleanup(srv.Close)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("sa-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return &hermesKube{base: srv.URL, ns: "hermes-agents", tokenFile: tokenFile, client: srv.Client()}, &calls, &lastBody
}

func TestStoreInferenceKeyCreatesSecret(t *testing.T) {
	k, calls, body := fakeKube(t, false, false)
	restarted, err := k.storeInferenceKey(context.Background(), "abc", "sk-x", time.Now())
	if err != nil || restarted {
		t.Fatalf("restarted=%v err=%v", restarted, err)
	}
	want := []string{
		"GET /api/v1/namespaces/hermes-agents/secrets/hermes-cred-abc",
		"POST /api/v1/namespaces/hermes-agents/secrets",
		"DELETE /api/v1/namespaces/hermes-agents/pods/hermes-agent-abc",
	}
	if len(*calls) != 3 || (*calls)[0] != want[0] || (*calls)[1] != want[1] || (*calls)[2] != want[2] {
		t.Fatalf("calls = %v", *calls)
	}
	if data, _ := (*body)["stringData"].(map[string]any); data["INFERENCE_KEY"] != "sk-x" {
		t.Fatalf("secret body = %v", *body)
	}
}

func TestStoreInferenceKeyPatchesSecretAndRestartsAgent(t *testing.T) {
	k, calls, _ := fakeKube(t, true, true)
	restarted, err := k.storeInferenceKey(context.Background(), "abc", "sk-x", time.Now())
	if err != nil || !restarted {
		t.Fatalf("restarted=%v err=%v", restarted, err)
	}
	if (*calls)[1] != "PATCH /api/v1/namespaces/hermes-agents/secrets/hermes-cred-abc" {
		t.Fatalf("calls = %v", *calls)
	}
}
