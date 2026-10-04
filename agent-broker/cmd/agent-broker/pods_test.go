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
		b := NewPodsBackend(&kube{base: srv.URL, ns: "agents", client: srv.Client()}, PodsConfig{Runtimes: []Runtime{{Name: "hermes", Image: "hermes"}}, WebSearch: on, Repowise: !on})
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
	b := NewPodsBackend(&kube{base: srv.URL, ns: "agents", client: srv.Client()},
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

func TestDshPodGetsWebPortAndWorkspace(t *testing.T) {
	var pod map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&pod)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte("{}"))
	}))
	defer srv.Close()
	b := NewPodsBackend(&kube{base: srv.URL, ns: "agents", client: srv.Client()},
		PodsConfig{Runtimes: []Runtime{{Name: "dsh", ModelID: "dsh-agent", Image: "dsh:1"}},
			MemLimit: "1Gi", DshMemLimit: "2Gi", DshWebHost: "dsh.example.com"})
	if err := b.createPod(context.Background(), User{ID: "abc", Name: "u"}, AgentRef{Runtime: "dsh", ID: "abc"}, StartSpec{CatalogImage: "catalog"}); err != nil {
		t.Fatal(err)
	}
	c := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
	env := map[string]any{}
	for _, e := range c["env"].([]any) {
		m := e.(map[string]any)
		env[m["name"].(string)] = m["value"]
	}
	if env["AGENT_CWD"] != "/opt/data/home/workspace" || env["DSH_WEB_HOST"] != "dsh.example.com" {
		t.Fatalf("env %v", env)
	}
	ports := c["ports"].([]any)
	if len(ports) != 2 || ports[1].(map[string]any)["containerPort"] != float64(DshWebPort) {
		t.Fatalf("ports %v", ports)
	}
	if mem := c["resources"].(map[string]any)["limits"].(map[string]any)["memory"]; mem != "2Gi" {
		t.Fatalf("memory limit %v", mem)
	}
}

func TestACPRuntimePods(t *testing.T) {
	podFor := func(cfg PodsConfig, runtime string) (map[string]any, map[string]any) {
		var pod map[string]any
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewDecoder(r.Body).Decode(&pod)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte("{}"))
		}))
		defer srv.Close()
		cfg.Runtimes = []Runtime{{Name: "hermes", ModelID: "hermes-agent", Image: "hermes:1"}, {Name: "pi", ModelID: "pi-agent", Image: "pi:1"}}
		b := NewPodsBackend(&kube{base: srv.URL, ns: "agents", client: srv.Client()}, cfg)
		if err := b.createPod(context.Background(), User{ID: "abc", Name: "u"}, AgentRef{Runtime: runtime, ID: "abc"}, StartSpec{CatalogImage: "catalog"}); err != nil {
			t.Fatal(err)
		}
		c := pod["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		env := map[string]any{}
		for _, e := range c["env"].([]any) {
			m := e.(map[string]any)
			env[m["name"].(string)] = m["value"]
		}
		return c, env
	}
	acp := PodsConfig{ACP: map[string]bool{"hermes": true, "pi": true}}

	c, env := podFor(acp, "hermes")
	if fmt.Sprint(c["command"]) != "[node /opt/agent/server.mjs]" || env["AGENT_RUNTIME"] != "hermes" ||
		env["AGENT_PROTOCOL"] != "acp" || env["HERMES_HOME"] != "/opt/data/home" {
		t.Fatalf("hermes over ACP: %v %v", c["command"], env)
	}
	if _, env := podFor(acp, "pi"); env["AGENT_PROTOCOL"] != "acp" {
		t.Fatalf("pi over ACP: %v", env)
	}
	c, env = podFor(PodsConfig{}, "hermes")
	if fmt.Sprint(c["command"]) != "[/bin/sh -c umask 002 && exec /opt/hermes/.venv/bin/hermes gateway run]" || env["AGENT_PROTOCOL"] != nil {
		t.Fatalf("native hermes: %v %v", c["command"], env)
	}
	if _, env := podFor(PodsConfig{}, "pi"); env["AGENT_PROTOCOL"] != nil {
		t.Fatalf("native pi: %v", env)
	}
}
