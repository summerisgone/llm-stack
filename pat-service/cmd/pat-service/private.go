package main

import (
	"net/http"
	"strings"
	"time"
)

// Private inference entry point for Open WebUI (docs/adr/0022 section 5).
// It serves the same /v1 API as the public PAT entry point but accepts the
// signed-in user's Keycloak access token instead of a PAT, so direct chat
// goes through the same session, band and ledger path as PAT traffic. It
// listens on its own port, which no HTTPRoute references and a
// NetworkPolicy opens only to the allowed callers; the public /v1 stays
// PAT-only (ADR 0003).

// privateSource labels ledger rows by the OIDC client that obtained the
// token.
func privateSource(azp string) string {
	if azp == "open-webui" {
		return "webui"
	}
	return "oidc:" + azp
}

func (a *app) privateProxy(w http.ResponseWriter, r *http.Request) {
	receivedAt := time.Now()
	if !strings.HasPrefix(r.URL.Path, "/v1/") {
		http.NotFound(w, r)
		return
	}
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" || strings.TrimSpace(token) != token || strings.HasPrefix(token, "sk-") {
		openAIError(w, 401, "Keycloak access token required")
		return
	}
	cl, err := a.verifyJWT(r.Context(), token, "")
	if err != nil {
		openAIError(w, 401, "Invalid authentication credentials")
		return
	}
	if !a.cfg.privateClients[cl.AuthorizedParty] {
		openAIError(w, 403, "this client may not use the private inference entry point")
		return
	}
	if !hasAIUserRole(cl.RealmAccess.Roles) {
		openAIError(w, 403, "AI Stack role is required")
		return
	}
	a.forwardInference(w, r, receivedAt, inferenceCaller{owner: cl.Subject, ownerName: cl.PreferredUsername, source: privateSource(cl.AuthorizedParty)})
}

func newPrivateMux(a *app) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/", a.privateProxy)
	mux.HandleFunc("POST /v1/", a.privateProxy)
	return mux
}

// coverage describes which inference paths the ledger covers for a period:
// "pat_only" before the operator recorded the Open WebUI cutover
// (INFERENCE_COVERAGE_SINCE), "all_paths" after it, "mixed" across it.
func (a *app) coverage(from time.Time) map[string]any {
	since := a.cfg.coverageSince
	switch {
	case since == nil:
		return map[string]any{"coverage": "pat_only", "coverage_since": nil}
	case !from.Before(*since):
		return map[string]any{"coverage": "all_paths", "coverage_since": since}
	}
	return map[string]any{"coverage": "mixed", "coverage_since": since}
}
