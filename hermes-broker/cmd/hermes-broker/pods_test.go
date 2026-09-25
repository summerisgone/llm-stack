package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSyncContainerGetsMCPSwitches(t *testing.T) {
	for _, on := range []bool{false, true} {
		var pod map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&pod)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
		}))
		b := NewPodsBackend(&kube{base: srv.URL, ns: "hermes-agents", client: srv.Client()}, PodsConfig{HermesImage: "hermes", WebSearch: on, Repowise: !on})
		if err := b.createPod(context.Background(), User{ID: "abc", Name: "u"}, StartSpec{CatalogImage: "catalog"}); err != nil {
			t.Fatal(err)
		}
		srv.Close()
		init := pod["spec"].(map[string]any)["initContainers"].([]any)[0].(map[string]any)
		got := map[string]string{}
		for _, e := range init["env"].([]any) {
			got[e.(map[string]any)["name"].(string)], _ = e.(map[string]any)["value"].(string)
		}
		if want := fmt.Sprint(on); got["WEB_SEARCH_ENABLED"] != want || got["REPOWISE_ENABLED"] != fmt.Sprint(!on) {
			t.Fatalf("WebSearch=%v: env %v", on, got)
		}
	}
}
