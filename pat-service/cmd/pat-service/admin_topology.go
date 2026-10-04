package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Admin topology screens: Inference, Nodes and Routes (docs/adr/0022
// sections 7-8). All read-only. Each response carries observed_at and the
// sources that failed, so a missing Prometheus or RBAC gap degrades a panel
// instead of the page.

func (a *app) staleAfter() time.Duration { return 2 * a.cfg.scrapeInterval }

// topologyErrors flattens per-source failures for the response.
func topologyErrors(snapErr error, snap *clusterSnapshot, prom map[string]string, promPrefix string) map[string]string {
	errs := map[string]string{}
	if snapErr != nil {
		errs["kubernetes"] = snapErr.Error()
	} else {
		for k, v := range snap.errs {
			errs["kubernetes/"+k] = v
		}
	}
	for k, v := range prom {
		errs[promPrefix+"/"+k] = v
	}
	return errs
}

func (a *app) adminInference(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	ctx := r.Context()
	snap, err := a.clusterSnapshot(ctx)
	health, promErrs := a.promBatch(ctx, a.cfg.gatewayPrometheusURL, envoyQueries)
	resp := map[string]any{"observed_at": time.Now().UTC(), "errors": topologyErrors(err, snap, promErrs, "gateway-prometheus"),
		"engines": []engineView{}, "pools": []poolView{}, "models": []modelRouteRule{}}
	if err == nil {
		engines := snap.engines()
		resp["engines"] = engines
		resp["pools"] = snap.poolViews(engines)
		resp["models"] = snap.modelRoutes(a.cfg.topologyNamespace, engines, buildRouteHealth(health, a.staleAfter(), time.Now()))
	}
	writeJSON(w, 200, resp)
}

type nodeView struct {
	Name            string        `json:"name"`
	Roles           []string      `json:"roles"`
	Ready           *bool         `json:"ready"`
	Unschedulable   bool          `json:"unschedulable"`
	Problems        []string      `json:"problems"`
	Taints          []string      `json:"taints"`
	KubeletVersion  string        `json:"kubelet_version"`
	KernelVersion   string        `json:"kernel_version"`
	AllocatableCPU  string        `json:"allocatable_cpu"`
	AllocatableMem  string        `json:"allocatable_memory"`
	GPUsAllocatable int           `json:"gpus_allocatable"`
	GPUsRequested   int           `json:"gpus_requested"`
	Engines         []nodeEngine  `json:"engines"`
	Hardware        *nodeHardware `json:"hardware"`
}

type nodeEngine struct {
	Engine         string `json:"engine"`
	Pod            string `json:"pod"`
	Ready          bool   `json:"ready"`
	GPUs           int    `json:"gpus"`
	TensorParallel int    `json:"tensor_parallel"`
	ModelCache     string `json:"model_cache"`
}

func nodeRoles(labels map[string]string) []string {
	roles := []string{}
	for k, v := range labels {
		switch {
		case strings.HasPrefix(k, "node-role.kubernetes.io/"):
			roles = append(roles, strings.TrimPrefix(k, "node-role.kubernetes.io/"))
		case strings.HasPrefix(k, "node-role/") && v == "true":
			roles = append(roles, strings.TrimPrefix(k, "node-role/"))
		}
	}
	sort.Strings(roles)
	return roles
}

func (s *clusterSnapshot) nodeViews(engines []engineView, hardware map[string]*nodeHardware) []nodeView {
	out := []nodeView{}
	for _, n := range s.nodes {
		v := nodeView{Name: n.Metadata.Name, Roles: nodeRoles(n.Metadata.Labels), Ready: conditionTrue(n.Status.Conditions, "Ready"),
			Unschedulable: n.Spec.Unschedulable, Problems: []string{}, Taints: []string{}, Engines: []nodeEngine{},
			KubeletVersion: n.Status.NodeInfo.KubeletVersion, KernelVersion: n.Status.NodeInfo.KernelVersion,
			AllocatableCPU: n.Status.Allocatable["cpu"], AllocatableMem: n.Status.Allocatable["memory"], Hardware: hardware[n.Metadata.Name]}
		v.GPUsAllocatable, _ = strconv.Atoi(n.Status.Allocatable["nvidia.com/gpu"])
		for _, c := range n.Status.Conditions {
			if c.Type != "Ready" && c.Status == "True" {
				v.Problems = append(v.Problems, c.Type)
			}
		}
		for _, t := range n.Spec.Taints {
			v.Taints = append(v.Taints, t.Key+":"+t.Effect)
		}
		for _, p := range s.pods {
			if p.Spec.NodeName == n.Metadata.Name && p.Status.Phase != "Succeeded" && p.Status.Phase != "Failed" {
				v.GPUsRequested += gpuCount(p.Spec.Containers)
			}
		}
		for _, e := range engines {
			for _, p := range e.Pods {
				if p.Node == n.Metadata.Name {
					v.Engines = append(v.Engines, nodeEngine{Engine: e.Name, Pod: p.Name, Ready: p.Ready, GPUs: p.GPUs, TensorParallel: e.TensorParallel, ModelCache: p.ModelCache})
				}
			}
		}
		out = append(out, v)
	}
	return out
}

func (a *app) nodesAndHardware(ctx context.Context, snap *clusterSnapshot, err error) (map[string]any, []nodeView) {
	results, promErrs := a.promBatch(ctx, a.cfg.prometheusURL, hardwareQueries)
	hardware := buildHardware(results, a.staleAfter(), time.Now())
	resp := map[string]any{"observed_at": time.Now().UTC(), "errors": topologyErrors(err, snap, promErrs, "prometheus"),
		"stale_after_seconds": a.staleAfter().Seconds(), "nodes": []nodeView{}}
	var nodes []nodeView
	if err == nil {
		nodes = snap.nodeViews(snap.engines(), hardware)
		resp["nodes"] = nodes
	}
	return resp, nodes
}

func (a *app) adminNodes(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	snap, err := a.clusterSnapshot(r.Context())
	resp, _ := a.nodesAndHardware(r.Context(), snap, err)
	writeJSON(w, 200, resp)
}

type edgeRouteRule struct {
	Index        int          `json:"index"`
	Name         string       `json:"name"`
	Matches      []string     `json:"matches"`
	Filters      []string     `json:"filters"`
	Backends     []string     `json:"backends"`
	Timeout      string       `json:"timeout"`
	EnvoyCluster string       `json:"envoy_cluster"`
	Health       *routeHealth `json:"health"`
}

type parentStatus struct {
	Parent   string `json:"parent"`
	Accepted *bool  `json:"accepted"`
	Resolved *bool  `json:"resolved_refs"`
	Reason   string `json:"reason"`
}

type edgeRoute struct {
	Name      string          `json:"name"`
	Parents   []string        `json:"parents"`
	Hostnames []string        `json:"hostnames"`
	Status    []parentStatus  `json:"status"`
	Rules     []edgeRouteRule `json:"rules"`
}

type policyView struct {
	Kind     string   `json:"kind"`
	Name     string   `json:"name"`
	Targets  []string `json:"targets"`
	Features []string `json:"features"`
	Accepted *bool    `json:"accepted"`
}

type gatewayView struct {
	Name       string   `json:"name"`
	Listeners  []string `json:"listeners"`
	Programmed *bool    `json:"programmed"`
}

func refString(r kubeRef) string {
	s := r.Kind
	if s == "" {
		s = "Service"
	}
	s += "/" + r.Name
	if r.SectionName != "" {
		s += "#" + r.SectionName
	}
	if r.Port != 0 {
		s += fmt.Sprintf(":%d", r.Port)
	}
	return s
}

// generatedByAIGateway reports HTTPRoutes the AI Gateway controller writes
// for an AIGatewayRoute of the same name; they are shown under that route.
func (s *clusterSnapshot) generatedByAIGateway(name string) bool {
	for _, r := range s.aiRoutes {
		if r.Metadata.Name == name {
			return true
		}
	}
	return false
}

func (s *clusterSnapshot) edgeRoutes(ns string, health map[string]routeHealth) []edgeRoute {
	out := []edgeRoute{}
	for _, route := range s.httpRoutes {
		if s.generatedByAIGateway(route.Metadata.Name) {
			continue
		}
		e := edgeRoute{Name: route.Metadata.Name, Parents: []string{}, Hostnames: route.Spec.Hostnames, Status: []parentStatus{}, Rules: []edgeRouteRule{}}
		if e.Hostnames == nil {
			e.Hostnames = []string{}
		}
		for _, p := range route.Spec.ParentRefs {
			e.Parents = append(e.Parents, refString(p))
		}
		// Per parent: the controller can keep status for a parentRef that no
		// longer matches a listener, which is worth seeing.
		for _, p := range route.Status.Parents {
			ps := parentStatus{Parent: refString(p.ParentRef), Accepted: conditionTrue(p.Conditions, "Accepted"), Resolved: conditionTrue(p.Conditions, "ResolvedRefs")}
			for _, c := range p.Conditions {
				if c.Status != "True" {
					ps.Reason = c.Reason
				}
			}
			e.Status = append(e.Status, ps)
		}
		for i, rule := range route.Spec.Rules {
			er := edgeRouteRule{Index: i, Name: rule.Name, Matches: []string{}, Filters: []string{}, Backends: []string{}, Timeout: rule.Timeouts.Request,
				EnvoyCluster: fmt.Sprintf("httproute/%s/%s/rule/%d", ns, route.Metadata.Name, i)}
			for _, m := range rule.Matches {
				match := ""
				if m.Path != nil {
					match = m.Path.Type + " " + m.Path.Value
				}
				for _, h := range m.Headers {
					match += " " + h.Name + "=" + h.Value
				}
				er.Matches = append(er.Matches, strings.TrimSpace(match))
			}
			for _, f := range rule.Filters {
				switch {
				case f.URLRewrite != nil && f.URLRewrite.Path != nil:
					er.Filters = append(er.Filters, "rewrite path -> "+f.URLRewrite.Path.ReplacePrefixMatch+f.URLRewrite.Path.ReplaceFullPath)
				case f.RequestHeaderModifier != nil:
					names := append([]string{}, f.RequestHeaderModifier.Remove...)
					for _, h := range f.RequestHeaderModifier.Set {
						names = append(names, "set "+h.Name)
					}
					for _, h := range f.RequestHeaderModifier.Add {
						names = append(names, "add "+h.Name)
					}
					er.Filters = append(er.Filters, "headers: "+strings.Join(names, ", "))
				case f.ExtensionRef != nil:
					er.Filters = append(er.Filters, refString(*f.ExtensionRef))
				default:
					er.Filters = append(er.Filters, f.Type)
				}
			}
			for _, b := range rule.BackendRefs {
				er.Backends = append(er.Backends, refString(b))
			}
			if h, ok := health[er.EnvoyCluster]; ok {
				er.Health = &h
			}
			e.Rules = append(e.Rules, er)
		}
		out = append(out, e)
	}
	return out
}

func policyViews(kind string, items []kubePolicy) []policyView {
	out := []policyView{}
	for _, p := range items {
		v := policyView{Kind: kind, Name: p.Metadata.Name, Targets: []string{}, Features: []string{}}
		for key, raw := range p.Spec {
			switch key {
			case "targetRef", "targetRefs":
				var refs []kubeRef
				if key == "targetRef" {
					var one kubeRef
					if decodeRaw(raw, &one) {
						refs = []kubeRef{one}
					}
				} else {
					decodeRaw(raw, &refs)
				}
				for _, r := range refs {
					v.Targets = append(v.Targets, refString(r))
				}
			default:
				v.Features = append(v.Features, key)
			}
		}
		sort.Strings(v.Features)
		for _, anc := range p.Status.Ancestors {
			v.Accepted = conditionTrue(anc.Conditions, "Accepted")
		}
		out = append(out, v)
	}
	return out
}

func (s *clusterSnapshot) gatewayViews() []gatewayView {
	out := []gatewayView{}
	for _, g := range s.gateways {
		v := gatewayView{Name: g.Metadata.Name, Listeners: []string{}, Programmed: conditionTrue(g.Status.Conditions, "Programmed")}
		for _, l := range g.Spec.Listeners {
			v.Listeners = append(v.Listeners, fmt.Sprintf("%s %s:%d", l.Name, l.Protocol, l.Port))
		}
		out = append(out, v)
	}
	return out
}

func (a *app) adminRoutes(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := a.adminGuard(w, r); !ok {
		return
	}
	ctx := r.Context()
	snap, err := a.clusterSnapshot(ctx)
	results, promErrs := a.promBatch(ctx, a.cfg.gatewayPrometheusURL, envoyQueries)
	health := buildRouteHealth(results, a.staleAfter(), time.Now())
	resp := map[string]any{
		"observed_at": time.Now().UTC(), "errors": topologyErrors(err, snap, promErrs, "gateway-prometheus"),
		// Kubernetes desired state and controller acceptance only: no
		// collector reads Envoy's effective configuration yet.
		"effective_config": "unavailable",
		"gateways":         []gatewayView{}, "edge_routes": []edgeRoute{}, "model_routes": []modelRouteRule{}, "policies": []policyView{},
	}
	if err == nil {
		ns := a.cfg.topologyNamespace
		resp["gateways"] = snap.gatewayViews()
		resp["edge_routes"] = snap.edgeRoutes(ns, health)
		resp["model_routes"] = snap.modelRoutes(ns, snap.engines(), health)
		resp["policies"] = append(policyViews("SecurityPolicy", snap.security), policyViews("BackendTrafficPolicy", snap.traffic)...)
	} else {
		log.Printf("admin routes: %v", err)
	}
	writeJSON(w, 200, resp)
}

func decodeRaw(raw []byte, out any) bool { return json.Unmarshal(raw, out) == nil }
