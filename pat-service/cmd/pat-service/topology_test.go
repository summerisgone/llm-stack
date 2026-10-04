package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func decodeFixture[T any](t *testing.T, raw string) []T {
	t.Helper()
	var list kubeList[T]
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		t.Fatal(err)
	}
	return list.Items
}

// fixtureSnapshot mirrors the stack's real shape: vLLM and SGLang share the
// qwen pool behind an endpoint picker, ninfer is a direct backend, an
// external API has no in-cluster service.
func fixtureSnapshot(t *testing.T) *clusterSnapshot {
	return &clusterSnapshot{
		deployments: decodeFixture[kubeDeployment](t, `{"items":[
		 {"metadata":{"name":"vllm","annotations":{"meta.helm.sh/release-name":"vllm-inference","kubectl.kubernetes.io/last-applied-configuration":"{\"secret\":\"leak\"}"}},
		  "spec":{"replicas":2,"selector":{"matchLabels":{"app":"vllm"}},"template":{"metadata":{"labels":{"app":"vllm","llm-d.ai/model":"qwen","llm-d.ai/engine-type":"vllm"}},
		   "spec":{"nodeSelector":{"node-role/inference":"true"},"initContainers":[{"name":"model-cache","command":["sh","/scripts/fill.sh","Qwen-NVFP4"]}],
		    "containers":[{"name":"vllm","image":"reg/vllm:0.27.1","args":["/model","--served-model-name","qwen","--tensor-parallel-size","2"],
		     "env":[{"name":"HF_TOKEN","value":"hf_secret"}],"resources":{"limits":{"nvidia.com/gpu":"2"}}}]}}},
		  "status":{"readyReplicas":1}},
		 {"metadata":{"name":"sglang","annotations":{"meta.helm.sh/release-name":"sglang-inference"}},
		  "spec":{"replicas":0,"selector":{"matchLabels":{"app":"sglang"}},"template":{"metadata":{"labels":{"app":"sglang","llm-d.ai/model":"qwen","llm-d.ai/engine-type":"sglang"}},
		   "spec":{"containers":[{"name":"sglang","image":"reg/sglang:v0.5@sha256:abc","command":["bash","-lc"],"args":["exec sglang serve --served-model-name \"qwen\" --tp-size 1"],"resources":{"limits":{"nvidia.com/gpu":"1"}}}]}}}},
		 {"metadata":{"name":"ninfer","annotations":{"meta.helm.sh/release-name":"ninfer-inference"}},
		  "spec":{"replicas":1,"selector":{"matchLabels":{"app":"ninfer"}},"template":{"metadata":{"labels":{"app":"ninfer"}},
		   "spec":{"containers":[{"name":"ninfer","image":"reg/ninfer:abc","args":["/model/x","--model-id","qwen-ninfer","--api-key","$(NINFER_API_KEY)"],"resources":{"limits":{"nvidia.com/gpu":"1"}}}]}}},
		  "status":{"readyReplicas":1}},
		 {"metadata":{"name":"keycloak"},"spec":{"replicas":1,"selector":{"matchLabels":{"app":"keycloak"}},"template":{"metadata":{"labels":{"app":"keycloak"}},"spec":{"containers":[{"name":"kc","image":"kc:26"}]}}}}
		]}`),
		pods: decodeFixture[kubePod](t, `{"items":[
		 {"metadata":{"name":"vllm-a","labels":{"app":"vllm"}},"spec":{"nodeName":"gpu-1","containers":[{"name":"vllm","resources":{"limits":{"nvidia.com/gpu":"2"}}}]},
		  "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}],"initContainerStatuses":[{"name":"model-cache","state":{"terminated":{"exitCode":0,"reason":"Completed"}}}]}},
		 {"metadata":{"name":"vllm-b","labels":{"app":"vllm"}},"spec":{"nodeName":"gpu-2","containers":[{"name":"vllm","resources":{"limits":{"nvidia.com/gpu":"2"}}}]},
		  "status":{"phase":"Pending","conditions":[{"type":"Ready","status":"False"}],"initContainerStatuses":[{"name":"model-cache","state":{"running":{}}}]}},
		 {"metadata":{"name":"ninfer-a","labels":{"app":"ninfer"}},"spec":{"nodeName":"gpu-3","containers":[{"name":"ninfer","resources":{"limits":{"nvidia.com/gpu":"1"}}}]},
		  "status":{"phase":"Running","conditions":[{"type":"Ready","status":"True"}]}}
		]}`),
		pools:    decodeFixture[kubeInferencePool](t, `{"items":[{"metadata":{"name":"qwen-pool"},"spec":{"selector":{"matchLabels":{"llm-d.ai/model":"qwen"}},"targetPorts":[{"number":8000}],"endpointPickerRef":{"name":"qwen-epp","failureMode":"FailOpen"}}}]}`),
		services: decodeFixture[kubeService](t, `{"items":[{"metadata":{"name":"ninfer-svc"},"spec":{"selector":{"app":"ninfer"}}},{"metadata":{"name":"qwen-epp"},"spec":{"selector":{"app":"epp"}}}]}`),
		slices: decodeFixture[kubeEndpointSlice](t, `{"items":[
		 {"metadata":{"labels":{"kubernetes.io/service-name":"ninfer-svc"}},"endpoints":[{"conditions":{"ready":true}}]},
		 {"metadata":{"labels":{"kubernetes.io/service-name":"qwen-epp"}},"endpoints":[{"conditions":{"ready":true}},{"conditions":{"ready":false}}]}]}`),
		aiRoutes: decodeFixture[kubeAIGatewayRoute](t, `{"items":[{"metadata":{"name":"models"},"spec":{"rules":[
		 {"name":"catch-all","backendRefs":[{"name":"ext","modelNameOverride":"flash"}]},
		 {"name":"qwen","matches":[{"headers":[{"name":"x-ai-eg-model","type":"Exact","value":"qwen"}]}],"backendRefs":[{"name":"pool-backend"}],"timeouts":{"request":"10m"}},
		 {"name":"ninfer","matches":[{"headers":[{"name":"x-ai-eg-model","value":"qwen-ninfer"}]}],"backendRefs":[{"name":"ninfer-backend"}]},
		 {"name":"broken","matches":[{"headers":[{"name":"x-ai-eg-model","value":"ghost"}]}],"backendRefs":[{"name":"missing"}]}]},
		 "status":{"conditions":[{"type":"Accepted","status":"True"}]}}]}`),
		aiBackends: decodeFixture[kubeAIServiceBackend](t, `{"items":[
		 {"metadata":{"name":"ext"},"spec":{"backendRef":{"kind":"Backend","name":"ext"}}},
		 {"metadata":{"name":"pool-backend"},"spec":{"backendRef":{"kind":"Backend","name":"epp-proxy"}},"status":{"conditions":[{"type":"Accepted","status":"True"}]}},
		 {"metadata":{"name":"ninfer-backend"},"spec":{"backendRef":{"kind":"Backend","name":"ninfer"}}}]}`),
		backends: decodeFixture[kubeBackend](t, `{"items":[
		 {"metadata":{"name":"ext"},"spec":{"endpoints":[{"fqdn":{"hostname":"host.k3d.internal","port":8095}}]}},
		 {"metadata":{"name":"epp-proxy"},"spec":{"endpoints":[{"fqdn":{"hostname":"qwen-epp.stack.svc.cluster.local","port":8081}}]}},
		 {"metadata":{"name":"ninfer"},"spec":{"endpoints":[{"fqdn":{"hostname":"ninfer-svc.stack.svc.cluster.local","port":18080}}]}}]}`),
		errs: map[string]string{},
	}
}

func TestEnginesInventory(t *testing.T) {
	snap := fixtureSnapshot(t)
	engines := snap.engines()
	if len(engines) != 3 {
		t.Fatalf("engines = %+v (keycloak must not count)", engines)
	}
	byName := map[string]engineView{}
	for _, e := range engines {
		byName[e.Name] = e
	}
	v := byName["vllm"]
	if v.Engine != "vllm" || !v.Managed || v.EngineVersion != "0.27.1" || v.ServedModel != "qwen" || v.TensorParallel != 2 || v.GPUsPerReplica != 2 ||
		v.Checkpoint != "Qwen-NVFP4" || v.State != "degraded" || len(v.Pools) != 1 || len(v.Pods) != 2 || v.Pods[0].ModelCache != "ready" || v.Pods[1].ModelCache != "filling" {
		t.Errorf("vllm = %+v", v)
	}
	if s := byName["sglang"]; s.State != "stopped" || s.ServedModel != "qwen" || s.TensorParallel != 1 || s.EngineVersion != "v0.5" {
		t.Errorf("sglang = %+v", s)
	}
	if n := byName["ninfer"]; n.Engine != "ninfer" || n.ServedModel != "qwen-ninfer" || len(n.Pools) != 0 || n.State != "serving" {
		t.Errorf("ninfer = %+v", n)
	}
	raw, _ := json.Marshal(engines)
	for _, secret := range []string{"hf_secret", "NINFER_API_KEY", "api-key", "leak", "HF_TOKEN"} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("inventory leaks %q: %s", secret, raw)
		}
	}
}

func TestModelRoutesResolveToServingEngines(t *testing.T) {
	snap := fixtureSnapshot(t)
	engines := snap.engines()
	rules := snap.modelRoutes("stack", engines, map[string]routeHealth{"httproute/stack/models/rule/1": {RequestRate: ptr(2)}})
	if len(rules) != 4 {
		t.Fatalf("rules = %+v", rules)
	}
	catchAll, qwen, ninfer, broken := rules[0].Targets[0], rules[1].Targets[0], rules[2].Targets[0], rules[3].Targets[0]
	if rules[0].Model != "" || catchAll.Kind != "external" || catchAll.ModelOverride != "flash" {
		t.Errorf("catch-all = %+v", catchAll)
	}
	if rules[1].Model != "qwen" || qwen.Kind != "pool" || qwen.Pool != "qwen-pool" || len(qwen.Engines) != 2 || *qwen.ReadyEndpoints != 1 ||
		rules[1].Health == nil || *rules[1].Health.RequestRate != 2 || *rules[1].RouteAccepted != true {
		t.Errorf("qwen = %+v %+v", rules[1], qwen)
	}
	if ninfer.Kind != "engine" || len(ninfer.Engines) != 1 || ninfer.Engines[0] != "ninfer" || *ninfer.ReadyEndpoints != 1 {
		t.Errorf("ninfer = %+v", ninfer)
	}
	if broken.Kind != "unresolved" {
		t.Errorf("missing backend = %+v", broken)
	}
	pools := snap.poolViews(engines)
	if len(pools) != 1 || pools[0].PickerReady != 1 || len(pools[0].Endpoints) != 2 {
		t.Errorf("pools = %+v", pools)
	}
}

func TestBuildHardwareDeduplicatesSharedHosts(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	s := func(v float64, labels ...string) promSample {
		m := map[string]string{}
		for i := 0; i+1 < len(labels); i += 2 {
			m[labels[i]] = labels[i+1]
		}
		return promSample{labels: m, value: v}
	}
	uname := func(node string) promSample {
		return s(1, "node", node, "release", "6.18-WSL2", "version", "#1 SMP")
	}
	r := map[string][]promSample{
		"load1":    {s(8, "node", "a"), s(8, "node", "b"), s(1, "node", "c")},
		"load_at":  {s(float64(now.Unix()-5), "node", "a"), s(float64(now.Unix()-5), "node", "b"), s(float64(now.Unix()-300), "node", "c")},
		"cpus":     {s(16, "node", "a"), s(16, "node", "b"), s(4, "node", "c")},
		"uname":    {uname("a"), uname("b"), uname("c")},
		"boot":     {s(1000, "node", "a"), s(1007, "node", "b"), s(2000, "node", "c")},
		"gpu_temp": {s(70, "node", "a", "gpu", "0", "name", "RTX"), s(70, "node", "b", "gpu", "0", "name", "RTX")},
		"hwmon":    {s(55, "node", "c", "chip", "coretemp", "sensor", "temp1")},
	}
	hw := buildHardware(r, 30*time.Second, now)
	if len(hw) != 3 || hw["a"].HostKey != hw["b"].HostKey || hw["a"].HostKey == hw["c"].HostKey {
		t.Fatalf("host keys: %+v", hw)
	}
	if len(hw["a"].SharesHostWith) != 1 || hw["a"].SharesHostWith[0] != "b" || len(hw["c"].SharesHostWith) != 0 {
		t.Errorf("shares: %v %v", hw["a"].SharesHostWith, hw["c"].SharesHostWith)
	}
	shared := 0
	for _, node := range []string{"a", "b"} {
		for _, g := range hw[node].GPUs {
			if g.SharedWithNode != "" {
				shared++
			}
		}
	}
	if shared != 1 {
		t.Errorf("one of the two GPU readings must point at the other: %+v %+v", hw["a"].GPUs, hw["b"].GPUs)
	}
	if !near(hw["a"].NormalizedLoad, 0.5) || hw["a"].Stale || !hw["c"].Stale {
		t.Errorf("load/stale: %+v %+v", hw["a"], hw["c"])
	}
	if len(hw["c"].Sensors) != 1 || len(hw["a"].Sensors) != 0 || hw["a"].MemTotal != nil {
		t.Errorf("missing sensors must stay empty, not zero: %+v", hw["a"])
	}
}
