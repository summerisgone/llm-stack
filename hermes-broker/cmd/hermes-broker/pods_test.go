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
		b := NewPodsBackend(&kube{base: srv.URL, ns: "hermes-agents", client: srv.Client()}, PodsConfig{Runtimes: []Runtime{{Name: "hermes", Image: "hermes"}}, WebSearch: on, Repowise: !on})
		if err := b.createPod(context.Background(), User{ID: "abc", Name: "u"}, AgentRef{Runtime: "hermes", ID: "abc"}, StartSpec{CatalogImage: "catalog"}); err != nil {
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

func TestAdapterRuntimePod(t *testing.T) {
	var pod map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&pod)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	b := NewPodsBackend(&kube{base: srv.URL, ns: "hermes-agents", client: srv.Client()},
		PodsConfig{Runtimes: []Runtime{{Name: "hermes", Image: "hermes"}, {Name: "pi", ModelID: "pi-agent", Image: "pi:1"}}})
	ref := AgentRef{Runtime: "pi", ID: "abc"}
	if err := b.createPod(context.Background(), User{ID: "abc", Name: "u"}, ref, StartSpec{CatalogImage: "catalog"}); err != nil {
		t.Fatal(err)
	}
	meta := pod["metadata"].(map[string]any)
	if meta["name"] != "pi-agent-abc" || meta["labels"].(map[string]any)[labelComponent] != "pi-agent" {
		t.Fatalf("metadata %v", meta)
	}
	spec := pod["spec"].(map[string]any)
	c := spec["containers"].([]any)[0].(map[string]any)
	if c["image"] != "pi:1" || c["command"] != nil {
		t.Fatalf("container %v", c)
	}
	env := map[string]any{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		env[m["name"].(string)] = m
	}
	for _, k := range []string{"AGENT_RUNTIME", "AGENT_INFERENCE_KEY", "API_SERVER_KEY", "AGENT_HOME"} {
		if env[k] == nil {
			t.Fatalf("missing env %s in %v", k, env)
		}
	}
	if env["HERMES_INFERENCE_KEY"] != nil {
		t.Fatal("hermes env on a pi agent")
	}
	vol := spec["volumes"].([]any)[0].(map[string]any)["persistentVolumeClaim"].(map[string]any)
	if vol["claimName"] != "pi-profile-abc" {
		t.Fatalf("volume %v", vol)
	}
}
