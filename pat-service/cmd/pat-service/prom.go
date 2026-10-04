package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"
)

// Fixed, bounded queries against the two existing Prometheus servers
// (docs/adr/0022 section 8). The browser only ever receives typed results:
// there is no PromQL pass-through.

type promSample struct {
	labels map[string]string
	value  float64
	at     time.Time
}

var errPromUnconfigured = errors.New("Prometheus is not configured")

func (a *app) promQuery(ctx context.Context, base, query string) ([]promSample, error) {
	if base == "" {
		return nil, errPromUnconfigured
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/query?query="+url.QueryEscape(query), nil)
	if err != nil {
		return nil, err
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Prometheus returned %s", resp.Status)
	}
	var out struct {
		Data struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	samples := make([]promSample, 0, len(out.Data.Result))
	for _, r := range out.Data.Result {
		ts, _ := r.Value[0].(float64)
		raw, _ := r.Value[1].(string)
		v, err := strconv.ParseFloat(raw, 64)
		if err != nil {
			continue
		}
		samples = append(samples, promSample{labels: r.Metric, value: v, at: time.Unix(0, int64(ts*1e9))})
	}
	return samples, nil
}

// promBatch runs named queries concurrently. A failed query is reported in
// errs and leaves its result empty; it never fails the others.
func (a *app) promBatch(ctx context.Context, base string, queries map[string]string) (map[string][]promSample, map[string]string) {
	results := map[string][]promSample{}
	errs := map[string]string{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for name, q := range queries {
		wg.Add(1)
		go func(name, q string) {
			defer wg.Done()
			samples, err := a.promQuery(ctx, base, q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs[name] = err.Error()
				return
			}
			results[name] = samples
		}(name, q)
	}
	wg.Wait()
	return results, errs
}

// hardwareQueries read node-exporter and gpu-exporter series from the
// application Prometheus. timestamp() gives the sample time for staleness.
var hardwareQueries = map[string]string{
	"load1":     `node_load1`,
	"load5":     `node_load5`,
	"load15":    `node_load15`,
	"load_at":   `timestamp(node_load1)`,
	"cpus":      `count by (node) (node_cpu_seconds_total{mode="idle"})`,
	"cpu_busy":  `1 - avg by (node) (rate(node_cpu_seconds_total{mode="idle"}[5m]))`,
	"mem_total": `node_memory_MemTotal_bytes`,
	"mem_avail": `node_memory_MemAvailable_bytes`,
	// k3d node containers expose no "/" mount; the kubelet directory sits
	// on the same disk.
	"disk_size":  `max by (node) (node_filesystem_size_bytes{mountpoint=~"/|/var/lib/kubelet"})`,
	"disk_avail": `min by (node) (node_filesystem_avail_bytes{mountpoint=~"/|/var/lib/kubelet"})`,
	"hwmon":      `max by (node, chip, sensor) (node_hwmon_temp_celsius)`,
	"thermal":    `max by (node, type) (node_thermal_zone_temp)`,
	"boot":       `node_boot_time_seconds`,
	"uname":      `node_uname_info`,
	"gpu_util":   `gpu_utilization_percent`,
	"gpu_mem":    `gpu_memory_used_bytes`,
	"gpu_memtot": `gpu_memory_total_bytes`,
	"gpu_temp":   `gpu_temperature_celsius`,
	"gpu_power":  `gpu_power_watts`,
	"gpu_at":     `timestamp(gpu_temperature_celsius)`,
}

// envoyQueries read data-plane health per Envoy cluster from the gateway
// add-ons Prometheus. Envoy Gateway names a route rule's cluster
// httproute/<namespace>/<route>/rule/<index>.
var envoyQueries = map[string]string{
	"rq_rate":  `sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_total{envoy_cluster_name=~"httproute/.*"}[5m]))`,
	"rq_5xx":   `sum by (envoy_cluster_name) (rate(envoy_cluster_upstream_rq_xx{envoy_cluster_name=~"httproute/.*",envoy_response_code_class="5"}[5m]))`,
	"healthy":  `sum by (envoy_cluster_name) (envoy_cluster_membership_healthy{envoy_cluster_name=~"httproute/.*"})`,
	"members":  `sum by (envoy_cluster_name) (envoy_cluster_membership_total{envoy_cluster_name=~"httproute/.*"})`,
	"observed": `max by (envoy_cluster_name) (timestamp(envoy_cluster_membership_total{envoy_cluster_name=~"httproute/.*"}))`,
}

// byLabel indexes samples by one label value.
func byLabel(samples []promSample, label string) map[string]promSample {
	out := map[string]promSample{}
	for _, s := range samples {
		out[s.labels[label]] = s
	}
	return out
}

type sensorReading struct {
	Label   string  `json:"label"`
	Celsius float64 `json:"celsius"`
}

type gpuReading struct {
	Index          string     `json:"index"`
	Name           string     `json:"name"`
	UtilPercent    *float64   `json:"util_percent"`
	MemUsedBytes   *float64   `json:"mem_used_bytes"`
	MemTotalBytes  *float64   `json:"mem_total_bytes"`
	TempCelsius    *float64   `json:"temp_celsius"`
	PowerWatts     *float64   `json:"power_watts"`
	ObservedAt     *time.Time `json:"observed_at"`
	Stale          bool       `json:"stale"`
	SharedWithNode string     `json:"shared_with_node,omitempty"`
}

type nodeHardware struct {
	HostKey        string          `json:"host_key"`
	SharesHostWith []string        `json:"shares_host_with"`
	Load1          *float64        `json:"load1"`
	Load5          *float64        `json:"load5"`
	Load15         *float64        `json:"load15"`
	CPUs           *float64        `json:"cpus"`
	NormalizedLoad *float64        `json:"normalized_load1"`
	CPUBusy        *float64        `json:"cpu_busy"`
	MemTotal       *float64        `json:"mem_total_bytes"`
	MemAvailable   *float64        `json:"mem_available_bytes"`
	DiskSize       *float64        `json:"disk_size_bytes"`
	DiskAvailable  *float64        `json:"disk_available_bytes"`
	Sensors        []sensorReading `json:"sensors"`
	GPUs           []gpuReading    `json:"gpus"`
	ObservedAt     *time.Time      `json:"observed_at"`
	Stale          bool            `json:"stale"`
}

func ptr(v float64) *float64 { return &v }

func sampleValue(m map[string]promSample, key string) *float64 {
	if s, ok := m[key]; ok {
		return ptr(s.value)
	}
	return nil
}

// buildHardware turns the hardwareQueries results into one entry per
// Kubernetes node. k3d nodes on one machine report the same host; they get
// the same host_key (kernel build plus boot time) so the UI can avoid
// counting shared hardware twice, and a GPU seen from several such nodes is
// listed once, on the first node, with the others pointing at it.
func buildHardware(r map[string][]promSample, staleAfter time.Duration, now time.Time) map[string]*nodeHardware {
	nodes := map[string]*nodeHardware{}
	get := func(node string) *nodeHardware {
		if nodes[node] == nil {
			nodes[node] = &nodeHardware{Sensors: []sensorReading{}, GPUs: []gpuReading{}, SharesHostWith: []string{}}
		}
		return nodes[node]
	}
	for _, name := range []string{"load1", "cpus", "mem_total", "uname"} {
		for _, s := range r[name] {
			get(s.labels["node"])
		}
	}
	delete(nodes, "")
	load1, load5, load15 := byLabel(r["load1"], "node"), byLabel(r["load5"], "node"), byLabel(r["load15"], "node")
	loadAt, cpus, busy := byLabel(r["load_at"], "node"), byLabel(r["cpus"], "node"), byLabel(r["cpu_busy"], "node")
	memT, memA := byLabel(r["mem_total"], "node"), byLabel(r["mem_avail"], "node")
	diskS, diskA := byLabel(r["disk_size"], "node"), byLabel(r["disk_avail"], "node")
	boot, uname := byLabel(r["boot"], "node"), byLabel(r["uname"], "node")
	type kernel struct {
		build string
		boot  float64
	}
	kernels := map[string]kernel{}
	for node, h := range nodes {
		h.Load1, h.Load5, h.Load15 = sampleValue(load1, node), sampleValue(load5, node), sampleValue(load15, node)
		h.CPUs, h.CPUBusy = sampleValue(cpus, node), sampleValue(busy, node)
		if h.Load1 != nil && h.CPUs != nil && *h.CPUs > 0 {
			h.NormalizedLoad = ptr(*h.Load1 / *h.CPUs)
		}
		h.MemTotal, h.MemAvailable = sampleValue(memT, node), sampleValue(memA, node)
		h.DiskSize, h.DiskAvailable = sampleValue(diskS, node), sampleValue(diskA, node)
		if s, ok := loadAt[node]; ok {
			at := time.Unix(int64(s.value), 0).UTC()
			h.ObservedAt, h.Stale = &at, now.Sub(at) > staleAfter
		}
		if u, ok := uname[node]; ok {
			if b, ok := boot[node]; ok {
				kernels[node] = kernel{u.labels["release"] + "|" + u.labels["version"], b.value}
			}
		}
	}
	// One running kernel = one machine: same build and boot time. The
	// reported boot time drifts by seconds between scrapes (WSL2), so it is
	// compared with a tolerance; the key uses the earliest node name.
	names := make([]string, 0, len(kernels))
	for node := range kernels {
		names = append(names, node)
	}
	sort.Strings(names)
	for _, node := range names {
		k := kernels[node]
		for _, other := range names {
			o := kernels[other]
			if o.build != k.build || math.Abs(o.boot-k.boot) > 120 {
				continue
			}
			if nodes[node].HostKey == "" {
				nodes[node].HostKey = "host-of-" + other
			}
			if other != node {
				nodes[node].SharesHostWith = append(nodes[node].SharesHostWith, other)
			}
		}
	}
	for _, s := range r["hwmon"] {
		if h := nodes[s.labels["node"]]; h != nil {
			h.Sensors = append(h.Sensors, sensorReading{Label: s.labels["chip"] + " " + s.labels["sensor"], Celsius: s.value})
		}
	}
	for _, s := range r["thermal"] {
		if h := nodes[s.labels["node"]]; h != nil {
			h.Sensors = append(h.Sensors, sensorReading{Label: "thermal " + s.labels["type"], Celsius: s.value})
		}
	}
	gpuKey := func(s promSample) string { return s.labels["node"] + "/" + s.labels["gpu"] }
	util, mem, memTot := map[string]promSample{}, map[string]promSample{}, map[string]promSample{}
	temp, power, at := map[string]promSample{}, map[string]promSample{}, map[string]promSample{}
	for name, dst := range map[string]map[string]promSample{"gpu_util": util, "gpu_mem": mem, "gpu_memtot": memTot, "gpu_temp": temp, "gpu_power": power, "gpu_at": at} {
		for _, s := range r[name] {
			dst[gpuKey(s)] = s
		}
	}
	seen := map[string]string{} // physical GPU -> node that lists it
	keys := make([]string, 0, len(temp))
	for key := range temp {
		keys = append(keys, key)
	}
	sort.Strings(keys) // the same node keeps the GPU on every refresh
	for _, key := range keys {
		t := temp[key]
		node := t.labels["node"]
		h := nodes[node]
		if h == nil {
			continue
		}
		g := gpuReading{Index: t.labels["gpu"], Name: t.labels["name"], TempCelsius: ptr(t.value),
			UtilPercent: sampleValue(util, key), MemUsedBytes: sampleValue(mem, key), MemTotalBytes: sampleValue(memTot, key), PowerWatts: sampleValue(power, key)}
		if s, ok := at[key]; ok {
			when := time.Unix(int64(s.value), 0).UTC()
			g.ObservedAt, g.Stale = &when, now.Sub(when) > staleAfter
		}
		physical := h.HostKey + "/" + g.Index + "/" + g.Name
		if h.HostKey != "" {
			if first, dup := seen[physical]; dup {
				g.SharedWithNode = first
			} else {
				seen[physical] = node
			}
		}
		h.GPUs = append(h.GPUs, g)
	}
	return nodes
}

type routeHealth struct {
	RequestRate *float64   `json:"request_rate"`
	ErrorRate   *float64   `json:"error_5xx_rate"`
	Healthy     *float64   `json:"healthy_endpoints"`
	Members     *float64   `json:"endpoints"`
	ObservedAt  *time.Time `json:"observed_at"`
	Stale       bool       `json:"stale"`
}

func buildRouteHealth(r map[string][]promSample, staleAfter time.Duration, now time.Time) map[string]routeHealth {
	rate, errs := byLabel(r["rq_rate"], "envoy_cluster_name"), byLabel(r["rq_5xx"], "envoy_cluster_name")
	healthy, members, observed := byLabel(r["healthy"], "envoy_cluster_name"), byLabel(r["members"], "envoy_cluster_name"), byLabel(r["observed"], "envoy_cluster_name")
	out := map[string]routeHealth{}
	for name := range members {
		h := routeHealth{RequestRate: sampleValue(rate, name), ErrorRate: sampleValue(errs, name), Healthy: sampleValue(healthy, name), Members: sampleValue(members, name)}
		if s, ok := observed[name]; ok {
			at := time.Unix(int64(s.value), 0).UTC()
			h.ObservedAt, h.Stale = &at, now.Sub(at) > staleAfter
		}
		out[name] = h
	}
	return out
}
