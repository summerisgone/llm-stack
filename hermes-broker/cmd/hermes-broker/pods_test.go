package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSyncContainerGetsWebSearchSwitch(t *testing.T) {
	for _, on := range []bool{false, true} {
		var pod map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&pod)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
		}))
		b := NewPodsBackend(&kube{base: srv.URL, ns: "hermes-agents", client: srv.Client()}, PodsConfig{HermesImage: "hermes", WebSearch: on})
		if err := b.createPod(context.Background(), User{ID: "abc", Name: "u"}, StartSpec{CatalogImage: "catalog"}); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		init := pod["spec"].(map[string]any)["initContainers"].([]any)[0].(map[string]any)
		got := ""
		for _, e := range init["env"].([]any) {
			if e.(map[string]any)["name"] == "WEB_SEARCH_ENABLED" {
				got = e.(map[string]any)["value"].(string)
			}
		}
		if want := map[bool]string{false: "false", true: "true"}[on]; got != want {
			t.Fatalf("WebSearch=%v: WEB_SEARCH_ENABLED=%q", on, got)
		}
	}
}
