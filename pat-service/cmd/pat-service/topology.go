package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Read-only cluster topology for the admin console (docs/adr/0022 sections
// 7-8): engines, pools, nodes and routes, built from Kubernetes objects in
// the stack namespace. Objects are decoded into the few fields shown here;
// env, annotations, secret references and full command lines are never read
// into these types, so they cannot reach an API response.

var errKubeUnavailable = errors.New("Kubernetes API is not available")

// get reads one API path with the pod's ServiceAccount (RBAC: pat-service-
// topology, read-only).
func (k *agentKube) get(ctx context.Context, path string, out any) error {
	token, err := os.ReadFile(k.tokenFile)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.base+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("GET %s: %d %s", path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type kubeMeta struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels"`
	// Only the Helm release name is read from annotations.
	Annotations struct {
		Release string `json:"meta.helm.sh/release-name"`
	} `json:"annotations"`
}

type kubeCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

type kubeContainer struct {
	Name      string   `json:"name"`
	Image     string   `json:"image"`
	Command   []string `json:"command"`
	Args      []string `json:"args"`
	Resources struct {
		Limits map[string]string `json:"limits"`
	} `json:"resources"`
}

type kubePodSpec struct {
	NodeName       string            `json:"nodeName"`
	Containers     []kubeContainer   `json:"containers"`
	InitContainers []kubeContainer   `json:"initContainers"`
	NodeSelector   map[string]string `json:"nodeSelector"`
}

type kubeContainerStatus struct {
	Name         string `json:"name"`
	Ready        bool   `json:"ready"`
	RestartCount int    `json:"restartCount"`
	State        struct {
		Waiting    *struct{ Reason string } `json:"waiting"`
		Running    *struct{}                `json:"running"`
		Terminated *struct {
			Reason   string `json:"reason"`
			ExitCode int    `json:"exitCode"`
		} `json:"terminated"`
	} `json:"state"`
}

type kubePod struct {
	Metadata kubeMeta    `json:"metadata"`
	Spec     kubePodSpec `json:"spec"`
	Status   struct {
		Phase                 string                `json:"phase"`
		Conditions            []kubeCondition       `json:"conditions"`
		InitContainerStatuses []kubeContainerStatus `json:"initContainerStatuses"`
		ContainerStatuses     []kubeContainerStatus `json:"containerStatuses"`
	} `json:"status"`
}

type kubeDeployment struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		Replicas *int32 `json:"replicas"`
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		Template struct {
			Metadata kubeMeta    `json:"metadata"`
			Spec     kubePodSpec `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
	Status struct {
		ReadyReplicas int32 `json:"readyReplicas"`
	} `json:"status"`
}

type kubeNode struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		Unschedulable bool `json:"unschedulable"`
		Taints        []struct {
			Key    string `json:"key"`
			Effect string `json:"effect"`
		} `json:"taints"`
	} `json:"spec"`
	Status struct {
		Conditions  []kubeCondition   `json:"conditions"`
		Allocatable map[string]string `json:"allocatable"`
		NodeInfo    struct {
			KubeletVersion string `json:"kubeletVersion"`
			KernelVersion  string `json:"kernelVersion"`
		} `json:"nodeInfo"`
	} `json:"status"`
}

type kubeService struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		Selector map[string]string `json:"selector"`
	} `json:"spec"`
}

type kubeEndpointSlice struct {
	Metadata  kubeMeta `json:"metadata"`
	Endpoints []struct {
		Conditions struct {
			Ready *bool `json:"ready"`
		} `json:"conditions"`
		TargetRef *struct {
			Name string `json:"name"`
		} `json:"targetRef"`
	} `json:"endpoints"`
}

type kubeInferencePool struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		Selector struct {
			MatchLabels map[string]string `json:"matchLabels"`
		} `json:"selector"`
		TargetPorts []struct {
			Number int `json:"number"`
		} `json:"targetPorts"`
		EndpointPickerRef struct {
			Name        string `json:"name"`
			FailureMode string `json:"failureMode"`
		} `json:"endpointPickerRef"`
	} `json:"spec"`
}

type kubeRef struct {
	Group       string `json:"group,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Name        string `json:"name"`
	SectionName string `json:"sectionName,omitempty"`
	Port        int    `json:"port,omitempty"`
	Weight      *int   `json:"weight,omitempty"`
}

type kubeAIGatewayRoute struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		ParentRefs []kubeRef `json:"parentRefs"`
		Rules      []struct {
			Name    string `json:"name"`
			Matches []struct {
				Headers []struct {
					Name  string `json:"name"`
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"matches"`
			BackendRefs []struct {
				Name              string `json:"name"`
				ModelNameOverride string `json:"modelNameOverride"`
				Weight            *int   `json:"weight"`
				Priority          *int   `json:"priority"`
			} `json:"backendRefs"`
			Timeouts struct {
				Request string `json:"request"`
			} `json:"timeouts"`
			StreamIdleTimeout string `json:"streamIdleTimeout"`
		} `json:"rules"`
	} `json:"spec"`
	Status struct {
		Conditions []kubeCondition `json:"conditions"`
	} `json:"status"`
}

type kubeAIServiceBackend struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		BackendRef kubeRef `json:"backendRef"`
		Schema     struct {
			Name   string `json:"name"`
			Prefix string `json:"prefix"`
		} `json:"schema"`
	} `json:"spec"`
	Status struct {
		Conditions []kubeCondition `json:"conditions"`
	} `json:"status"`
}

type kubeBackend struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		Endpoints []struct {
			FQDN *struct {
				Hostname string `json:"hostname"`
				Port     int    `json:"port"`
			} `json:"fqdn"`
			IP *struct {
				Address string `json:"address"`
				Port    int    `json:"port"`
			} `json:"ip"`
		} `json:"endpoints"`
	} `json:"spec"`
	Status struct {
		Conditions []kubeCondition `json:"conditions"`
	} `json:"status"`
}

type kubeHTTPRoute struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		ParentRefs []kubeRef `json:"parentRefs"`
		Hostnames  []string  `json:"hostnames"`
		Rules      []struct {
			Name    string `json:"name"`
			Matches []struct {
				Path *struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"path"`
				Headers []struct {
					Name  string `json:"name"`
					Value string `json:"value"`
				} `json:"headers"`
			} `json:"matches"`
			Filters []struct {
				Type       string `json:"type"`
				URLRewrite *struct {
					Path *struct {
						Type               string `json:"type"`
						ReplacePrefixMatch string `json:"replacePrefixMatch"`
						ReplaceFullPath    string `json:"replaceFullPath"`
					} `json:"path"`
				} `json:"urlRewrite"`
				// Header names only: set/add values can carry credentials.
				RequestHeaderModifier *struct {
					Remove []string `json:"remove"`
					Set    []struct {
						Name string `json:"name"`
					} `json:"set"`
					Add []struct {
						Name string `json:"name"`
					} `json:"add"`
				} `json:"requestHeaderModifier"`
				ExtensionRef *kubeRef `json:"extensionRef"`
			} `json:"filters"`
			BackendRefs []kubeRef `json:"backendRefs"`
			Timeouts    struct {
				Request string `json:"request"`
			} `json:"timeouts"`
		} `json:"rules"`
	} `json:"spec"`
	Status struct {
		Parents []struct {
			ParentRef  kubeRef         `json:"parentRef"`
			Conditions []kubeCondition `json:"conditions"`
		} `json:"parents"`
	} `json:"status"`
}

type kubeGateway struct {
	Metadata kubeMeta `json:"metadata"`
	Spec     struct {
		Listeners []struct {
			Name     string `json:"name"`
			Port     int    `json:"port"`
			Protocol string `json:"protocol"`
		} `json:"listeners"`
	} `json:"spec"`
	Status struct {
		Conditions []kubeCondition `json:"conditions"`
	} `json:"status"`
}

// kubePolicy keeps only target references and which features a policy
// configures (its spec keys), never their values.
type kubePolicy struct {
	Metadata kubeMeta                   `json:"metadata"`
	Spec     map[string]json.RawMessage `json:"spec"`
	Status   struct {
		Ancestors []struct {
			Conditions []kubeCondition `json:"conditions"`
		} `json:"ancestors"`
	} `json:"status"`
}

type kubeList[T any] struct {
	Items []T `json:"items"`
}

// clusterSnapshot is one consistent-enough read of everything the topology
// screens need. Each list that failed is named in errs; the rest still
// render.
type clusterSnapshot struct {
	nodes       []kubeNode
	pods        []kubePod
	deployments []kubeDeployment
	services    []kubeService
	slices      []kubeEndpointSlice
	pools       []kubeInferencePool
	aiRoutes    []kubeAIGatewayRoute
	aiBackends  []kubeAIServiceBackend
	backends    []kubeBackend
	httpRoutes  []kubeHTTPRoute
	gateways    []kubeGateway
	security    []kubePolicy
	traffic     []kubePolicy
	errs        map[string]string
}

func (a *app) clusterSnapshot(ctx context.Context) (*clusterSnapshot, error) {
	if a.agents == nil {
		return nil, errKubeUnavailable
	}
	ns := a.cfg.topologyNamespace
	snap := &clusterSnapshot{errs: map[string]string{}}
	var mu sync.Mutex
	var wg sync.WaitGroup
	fetch := func(name, path string, out any) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := a.agents.get(ctx, path, out); err != nil {
				mu.Lock()
				snap.errs[name] = err.Error()
				mu.Unlock()
			}
		}()
	}
	var (
		nodes      kubeList[kubeNode]
		pods       kubeList[kubePod]
		deploys    kubeList[kubeDeployment]
		services   kubeList[kubeService]
		slices     kubeList[kubeEndpointSlice]
		pools      kubeList[kubeInferencePool]
		aiRoutes   kubeList[kubeAIGatewayRoute]
		aiBackends kubeList[kubeAIServiceBackend]
		backends   kubeList[kubeBackend]
		httpRoutes kubeList[kubeHTTPRoute]
		gateways   kubeList[kubeGateway]
		security   kubeList[kubePolicy]
		traffic    kubeList[kubePolicy]
	)
	nsPath := func(api, resource string) string { return api + "/namespaces/" + ns + "/" + resource }
	fetch("nodes", "/api/v1/nodes", &nodes)
	fetch("pods", nsPath("/api/v1", "pods"), &pods)
	fetch("deployments", nsPath("/apis/apps/v1", "deployments"), &deploys)
	fetch("services", nsPath("/api/v1", "services"), &services)
	fetch("endpointslices", nsPath("/apis/discovery.k8s.io/v1", "endpointslices"), &slices)
	fetch("inferencepools", nsPath("/apis/inference.networking.k8s.io/v1", "inferencepools"), &pools)
	fetch("aigatewayroutes", nsPath("/apis/aigateway.envoyproxy.io/v1beta1", "aigatewayroutes"), &aiRoutes)
	fetch("aiservicebackends", nsPath("/apis/aigateway.envoyproxy.io/v1beta1", "aiservicebackends"), &aiBackends)
	fetch("backends", nsPath("/apis/gateway.envoyproxy.io/v1alpha1", "backends"), &backends)
	fetch("httproutes", nsPath("/apis/gateway.networking.k8s.io/v1", "httproutes"), &httpRoutes)
	fetch("gateways", nsPath("/apis/gateway.networking.k8s.io/v1", "gateways"), &gateways)
	fetch("securitypolicies", nsPath("/apis/gateway.envoyproxy.io/v1alpha1", "securitypolicies"), &security)
	fetch("backendtrafficpolicies", nsPath("/apis/gateway.envoyproxy.io/v1alpha1", "backendtrafficpolicies"), &traffic)
	wg.Wait()
	snap.nodes, snap.pods, snap.deployments, snap.services, snap.slices = nodes.Items, pods.Items, deploys.Items, services.Items, slices.Items
	snap.pools, snap.aiRoutes, snap.aiBackends, snap.backends = pools.Items, aiRoutes.Items, aiBackends.Items, backends.Items
	snap.httpRoutes, snap.gateways, snap.security, snap.traffic = httpRoutes.Items, gateways.Items, security.Items, traffic.Items
	return snap, nil
}

func selectorMatches(selector, labels map[string]string) bool {
	if len(selector) == 0 {
		return false
	}
	for k, v := range selector {
		if labels[k] != v {
			return false
		}
	}
	return true
}

func conditionTrue(conditions []kubeCondition, kind string) *bool {
	for _, c := range conditions {
		if c.Type == kind {
			ok := c.Status == "True"
			return &ok
		}
	}
	return nil
}

func gpuCount(containers []kubeContainer) int {
	n := 0
	for _, c := range containers {
		v, _ := strconv.Atoi(c.Resources.Limits["nvidia.com/gpu"])
		n += v
	}
	return n
}

var (
	servedModelFlag    = regexp.MustCompile(`--served-model-name[= ]+"?([^"\s\\]+)`)
	modelIDFlag        = regexp.MustCompile(`--model-id[= ]+"?([^"\s\\]+)`)
	tensorParallelFlag = regexp.MustCompile(`--(?:tensor-parallel-size|tp-size|tp)[= ]+"?(\d+)`)
)

// flagValue reads one whitelisted flag from a container's command line.
// Only the matched value leaves this function, never the command line.
func flagValue(c kubeContainer, re *regexp.Regexp) string {
	if m := re.FindStringSubmatch(strings.Join(append(append([]string{}, c.Command...), c.Args...), " ")); m != nil {
		return m[1]
	}
	return ""
}

func imageVersion(image string) string {
	image, _, _ = strings.Cut(image, "@")
	if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
		return image[i+1:]
	}
	return ""
}

// managedEngines are the engines ADR 0022 plans managed switching for;
// anything else is listed read-only.
var managedEngines = map[string]bool{"vllm": true, "sglang": true, "ninfer": true}

type enginePod struct {
	Name       string `json:"name"`
	Node       string `json:"node"`
	Phase      string `json:"phase"`
	Ready      bool   `json:"ready"`
	GPUs       int    `json:"gpus"`
	Restarts   int    `json:"restarts"`
	ModelCache string `json:"model_cache"`
}

type engineView struct {
	Name           string      `json:"name"`
	Release        string      `json:"release"`
	Engine         string      `json:"engine"`
	Managed        bool        `json:"managed"`
	Image          string      `json:"image"`
	EngineVersion  string      `json:"engine_version"`
	ServedModel    string      `json:"served_model"`
	Checkpoint     string      `json:"checkpoint"`
	GPUsPerReplica int         `json:"gpus_per_replica"`
	TensorParallel int         `json:"tensor_parallel"`
	Desired        int32       `json:"desired_replicas"`
	Ready          int32       `json:"ready_replicas"`
	State          string      `json:"state"`
	Pools          []string    `json:"pools"`
	Pods           []enginePod `json:"pods"`
	NodeSelector   []string    `json:"node_selector"`
}

func isEngine(d kubeDeployment) bool {
	t := d.Spec.Template
	return gpuCount(t.Spec.Containers) > 0 || t.Metadata.Labels["llm-d.ai/model"] != "" || strings.HasSuffix(d.Metadata.Annotations.Release, "-inference")
}

func initState(pod kubePod, name string) string {
	for _, s := range pod.Status.InitContainerStatuses {
		if s.Name != name {
			continue
		}
		switch {
		case s.State.Terminated != nil && s.State.Terminated.ExitCode == 0:
			return "ready"
		case s.State.Terminated != nil:
			return "failed: " + s.State.Terminated.Reason
		case s.State.Running != nil:
			return "filling"
		case s.State.Waiting != nil:
			return "waiting: " + s.State.Waiting.Reason
		}
	}
	return "unknown"
}

func (s *clusterSnapshot) engines() []engineView {
	out := []engineView{}
	for _, d := range s.deployments {
		if !isEngine(d) {
			continue
		}
		t := d.Spec.Template
		e := engineView{Name: d.Metadata.Name, Release: d.Metadata.Annotations.Release, Pools: []string{}, Pods: []enginePod{}, NodeSelector: []string{}, Ready: d.Status.ReadyReplicas}
		e.Engine = t.Metadata.Labels["llm-d.ai/engine-type"]
		if e.Engine == "" {
			e.Engine = strings.TrimSuffix(e.Release, "-inference")
		}
		if e.Engine == "" {
			e.Engine = "unknown"
		}
		e.Managed = managedEngines[e.Engine]
		if d.Spec.Replicas != nil {
			e.Desired = *d.Spec.Replicas
		} else {
			e.Desired = 1
		}
		if len(t.Spec.Containers) > 0 {
			main := t.Spec.Containers[0]
			e.Image, e.EngineVersion = main.Image, imageVersion(main.Image)
			e.ServedModel = flagValue(main, servedModelFlag)
			if e.ServedModel == "" {
				e.ServedModel = flagValue(main, modelIDFlag)
			}
			if tp, err := strconv.Atoi(flagValue(main, tensorParallelFlag)); err == nil {
				e.TensorParallel = tp
			}
		}
		if e.ServedModel == "" {
			e.ServedModel = t.Metadata.Labels["llm-d.ai/model"]
		}
		e.GPUsPerReplica = gpuCount(t.Spec.Containers)
		if e.TensorParallel == 0 {
			e.TensorParallel = max(e.GPUsPerReplica, 1)
		}
		for _, c := range t.Spec.InitContainers {
			// model-cache-fill.sh takes the checkpoint directory as its
			// last argument.
			if line := append(append([]string{}, c.Command...), c.Args...); c.Name == "model-cache" && len(line) > 0 {
				e.Checkpoint = line[len(line)-1]
			}
		}
		for k, v := range t.Spec.NodeSelector {
			e.NodeSelector = append(e.NodeSelector, k+"="+v)
		}
		for _, p := range s.pools {
			if selectorMatches(p.Spec.Selector.MatchLabels, t.Metadata.Labels) {
				e.Pools = append(e.Pools, p.Metadata.Name)
			}
		}
		for _, pod := range s.pods {
			if !selectorMatches(d.Spec.Selector.MatchLabels, pod.Metadata.Labels) {
				continue
			}
			ep := enginePod{Name: pod.Metadata.Name, Node: pod.Spec.NodeName, Phase: pod.Status.Phase, GPUs: gpuCount(pod.Spec.Containers), ModelCache: initState(pod, "model-cache")}
			if r := conditionTrue(pod.Status.Conditions, "Ready"); r != nil {
				ep.Ready = *r
			}
			for _, cs := range pod.Status.ContainerStatuses {
				ep.Restarts += cs.RestartCount
			}
			e.Pods = append(e.Pods, ep)
		}
		switch {
		case e.Desired == 0 && len(e.Pods) == 0:
			e.State = "stopped"
		case e.Desired == 0:
			e.State = "stopping"
		case e.Ready >= e.Desired:
			e.State = "serving"
		case e.Ready > 0:
			e.State = "degraded"
		default:
			e.State = "starting"
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

type poolView struct {
	Name           string            `json:"name"`
	Selector       map[string]string `json:"selector"`
	TargetPorts    []int             `json:"target_ports"`
	EndpointPicker string            `json:"endpoint_picker"`
	PickerReady    int               `json:"endpoint_picker_ready"`
	FailureMode    string            `json:"failure_mode"`
	Engines        []string          `json:"engines"`
	Endpoints      []enginePod       `json:"endpoints"`
}

func (s *clusterSnapshot) readyEndpoints(service string) int {
	n := 0
	for _, sl := range s.slices {
		if sl.Metadata.Labels["kubernetes.io/service-name"] != service {
			continue
		}
		for _, ep := range sl.Endpoints {
			if ep.Conditions.Ready == nil || *ep.Conditions.Ready {
				n++
			}
		}
	}
	return n
}

func (s *clusterSnapshot) poolViews(engines []engineView) []poolView {
	out := []poolView{}
	for _, p := range s.pools {
		v := poolView{Name: p.Metadata.Name, Selector: p.Spec.Selector.MatchLabels, TargetPorts: []int{}, EndpointPicker: p.Spec.EndpointPickerRef.Name,
			FailureMode: p.Spec.EndpointPickerRef.FailureMode, Engines: []string{}, Endpoints: []enginePod{}}
		for _, port := range p.Spec.TargetPorts {
			v.TargetPorts = append(v.TargetPorts, port.Number)
		}
		v.PickerReady = s.readyEndpoints(v.EndpointPicker)
		for _, e := range engines {
			for _, name := range e.Pools {
				if name == v.Name {
					v.Engines = append(v.Engines, e.Name)
					v.Endpoints = append(v.Endpoints, e.Pods...)
				}
			}
		}
		out = append(out, v)
	}
	return out
}

// serviceFromHost maps an in-cluster DNS name in the stack namespace to its
// Service name; "" for anything else (an external endpoint).
func serviceFromHost(host, ns string) string {
	parts := strings.Split(host, ".")
	if len(parts) >= 3 && parts[1] == ns && parts[2] == "svc" {
		return parts[0]
	}
	return ""
}

type routeTarget struct {
	Backend        string   `json:"backend"`
	BackendKind    string   `json:"backend_kind"`
	ModelOverride  string   `json:"model_override,omitempty"`
	Weight         *int     `json:"weight,omitempty"`
	Endpoints      []string `json:"endpoints"`
	Kind           string   `json:"kind"` // pool, engine, service, external, unresolved
	Pool           string   `json:"pool,omitempty"`
	Engines        []string `json:"engines"`
	ReadyEndpoints *int     `json:"ready_endpoints"`
	Accepted       *bool    `json:"accepted"`
}

type modelRouteRule struct {
	Route         string        `json:"route"`
	Rule          string        `json:"rule"`
	Index         int           `json:"index"`
	Model         string        `json:"model"` // empty for the catch-all rule
	Timeout       string        `json:"timeout"`
	StreamIdle    string        `json:"stream_idle_timeout"`
	Targets       []routeTarget `json:"targets"`
	EnvoyCluster  string        `json:"envoy_cluster"`
	Health        *routeHealth  `json:"health"`
	RouteAccepted *bool         `json:"route_accepted"`
}

// modelRoutes resolves every AIGatewayRoute rule down to what serves it:
// AIServiceBackend -> Backend endpoint -> Service -> InferencePool (via its
// endpoint picker) or engine Deployment.
func (s *clusterSnapshot) modelRoutes(ns string, engines []engineView, health map[string]routeHealth) []modelRouteRule {
	aiBackends := map[string]kubeAIServiceBackend{}
	for _, b := range s.aiBackends {
		aiBackends[b.Metadata.Name] = b
	}
	backends := map[string]kubeBackend{}
	for _, b := range s.backends {
		backends[b.Metadata.Name] = b
	}
	pickers := map[string]string{}
	for _, p := range s.pools {
		pickers[p.Spec.EndpointPickerRef.Name] = p.Metadata.Name
	}
	services := map[string]kubeService{}
	for _, svc := range s.services {
		services[svc.Metadata.Name] = svc
	}
	deployLabels := map[string]map[string]string{}
	for _, d := range s.deployments {
		deployLabels[d.Metadata.Name] = d.Spec.Template.Metadata.Labels
	}
	enginesInPool := func(pool string) []string {
		names := []string{}
		for _, e := range engines {
			for _, p := range e.Pools {
				if p == pool {
					names = append(names, e.Name)
				}
			}
		}
		return names
	}
	resolveService := func(t *routeTarget, svcName string) {
		if pool, ok := pickers[svcName]; ok {
			t.Kind, t.Pool, t.Engines = "pool", pool, enginesInPool(pool)
			n := 0
			for _, e := range engines {
				for _, p := range e.Pools {
					if p == pool {
						n += int(e.Ready)
					}
				}
			}
			t.ReadyEndpoints = &n
			return
		}
		t.Kind = "service"
		if svc, ok := services[svcName]; ok {
			for _, e := range engines {
				if selectorMatches(svc.Spec.Selector, deployLabels[e.Name]) {
					t.Kind = "engine"
					t.Engines = append(t.Engines, e.Name)
				}
			}
		}
		n := s.readyEndpoints(svcName)
		t.ReadyEndpoints = &n
	}
	out := []modelRouteRule{}
	for _, route := range s.aiRoutes {
		accepted := conditionTrue(route.Status.Conditions, "Accepted")
		for i, rule := range route.Spec.Rules {
			r := modelRouteRule{Route: route.Metadata.Name, Rule: rule.Name, Index: i, Timeout: rule.Timeouts.Request, StreamIdle: rule.StreamIdleTimeout,
				Targets: []routeTarget{}, RouteAccepted: accepted, EnvoyCluster: fmt.Sprintf("httproute/%s/%s/rule/%d", ns, route.Metadata.Name, i)}
			for _, m := range rule.Matches {
				for _, h := range m.Headers {
					if strings.EqualFold(h.Name, "x-ai-eg-model") {
						r.Model = h.Value
					}
				}
			}
			for _, ref := range rule.BackendRefs {
				t := routeTarget{Backend: ref.Name, ModelOverride: ref.ModelNameOverride, Weight: ref.Weight, Endpoints: []string{}, Engines: []string{}, Kind: "unresolved"}
				if ab, ok := aiBackends[ref.Name]; ok {
					t.Accepted = conditionTrue(ab.Status.Conditions, "Accepted")
					t.BackendKind = ab.Spec.BackendRef.Kind
					switch ab.Spec.BackendRef.Kind {
					case "InferencePool":
						t.Kind, t.Pool, t.Engines = "pool", ab.Spec.BackendRef.Name, enginesInPool(ab.Spec.BackendRef.Name)
					case "Backend":
						for _, ep := range backends[ab.Spec.BackendRef.Name].Spec.Endpoints {
							switch {
							case ep.FQDN != nil:
								t.Endpoints = append(t.Endpoints, fmt.Sprintf("%s:%d", ep.FQDN.Hostname, ep.FQDN.Port))
								if svc := serviceFromHost(ep.FQDN.Hostname, ns); svc != "" {
									resolveService(&t, svc)
								} else {
									t.Kind = "external"
								}
							case ep.IP != nil:
								t.Endpoints = append(t.Endpoints, fmt.Sprintf("%s:%d", ep.IP.Address, ep.IP.Port))
								t.Kind = "external"
							}
						}
					case "Service":
						resolveService(&t, ab.Spec.BackendRef.Name)
					}
				}
				r.Targets = append(r.Targets, t)
			}
			if h, ok := health[r.EnvoyCluster]; ok {
				r.Health = &h
			}
			out = append(out, r)
		}
	}
	return out
}
