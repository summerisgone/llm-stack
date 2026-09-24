package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	labelUser      = "hermes.llm-stack/user-id"
	labelComponent = "app.kubernetes.io/component"
	labelManagedBy = "app.kubernetes.io/managed-by"
	annActivity    = "hermes.llm-stack/last-activity"
	annCatalog     = "hermes.llm-stack/catalog-image"
	annSelection   = "hermes.llm-stack/selection"
	annReport      = "hermes.llm-stack/sync-report"
	annUsername    = "hermes.llm-stack/username"
	syncContainer  = "catalog-sync"
)

var errNotFound = errors.New("not found")

// kube is a minimal in-cluster Kubernetes REST client: the broker needs a
// handful of namespaced calls, not client-go.
type kube struct {
	base   string
	ns     string
	token  string
	client *http.Client
}

func newInClusterKube(ns string) (*kube, error) {
	const sa = "/var/run/secrets/kubernetes.io/serviceaccount/"
	token, err := os.ReadFile(sa + "token")
	if err != nil {
		return nil, err
	}
	ca, err := os.ReadFile(sa + "ca.crt")
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca)
	host := os.Getenv("KUBERNETES_SERVICE_HOST")
	port := os.Getenv("KUBERNETES_SERVICE_PORT")
	return &kube{
		base:  "https://" + strings.Trim(host, "[]") + ":" + port,
		ns:    ns,
		token: strings.TrimSpace(string(token)),
		client: &http.Client{Timeout: 30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}},
	}, nil
}

func (k *kube) path(resource, name string) string {
	p := "/api/v1/namespaces/" + k.ns + "/" + resource
	if name != "" {
		p += "/" + name
	}
	return p
}

func (k *kube) do(ctx context.Context, method, path, contentType string, body, out any) error {
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, k.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+k.token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return errNotFound
	}
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("%s %s: %d %s", method, path, resp.StatusCode, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (k *kube) patchAnnotations(ctx context.Context, resource, name string, ann map[string]string) error {
	body := map[string]any{"metadata": map[string]any{"annotations": ann}}
	return k.do(ctx, http.MethodPatch, k.path(resource, name), "application/merge-patch+json", body, nil)
}

type PodsConfig struct {
	HermesImage   string
	RuntimeClass  string // e.g. gvisor; empty = cluster default
	StorageClass  string // empty = cluster default
	ProfileSize   string
	CPURequest    string
	CPULimit      string
	MemRequest    string
	MemLimit      string
	WorkSize      string
	StartTimeout  time.Duration
	StopGrace     int64
	ExtraAgentEnv map[string]string
}

// PodsBackend: one pod per active user, mounting only that user's RWO PVC.
// Pod, PVC and Secret names are derived from the profile id.
type PodsBackend struct {
	k   *kube
	cfg PodsConfig
}

func NewPodsBackend(k *kube, cfg PodsConfig) *PodsBackend { return &PodsBackend{k: k, cfg: cfg} }

func podName(id string) string    { return "hermes-agent-" + id }
func pvcName(id string) string    { return "hermes-profile-" + id }
func secretName(id string) string { return "hermes-cred-" + id }

func runtimeLabels(id string) map[string]string {
	return map[string]string{
		labelUser:                id,
		labelManagedBy:           "hermes-broker",
		"app.kubernetes.io/name": "hermes",
	}
}

func (b *PodsBackend) EnsureProfile(ctx context.Context, u User) (bool, error) {
	err := b.k.do(ctx, http.MethodGet, b.k.path("persistentvolumeclaims", pvcName(u.ID)), "", nil, nil)
	if err == nil {
		return false, nil
	}
	if !errors.Is(err, errNotFound) {
		return false, err
	}
	labels := runtimeLabels(u.ID)
	labels[labelComponent] = "hermes-profile"
	spec := map[string]any{
		"accessModes": []string{"ReadWriteOnce"},
		"resources":   map[string]any{"requests": map[string]string{"storage": b.cfg.ProfileSize}},
	}
	if b.cfg.StorageClass != "" {
		spec["storageClassName"] = b.cfg.StorageClass
	}
	pvc := map[string]any{
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": map[string]any{"name": pvcName(u.ID), "labels": labels,
			"annotations": map[string]string{annUsername: u.Name}},
		"spec": spec,
	}
	if err := b.k.do(ctx, http.MethodPost, b.k.path("persistentvolumeclaims", ""), "application/json", pvc, nil); err != nil {
		if strings.Contains(err.Error(), ": 409 ") {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// credential returns the key the broker uses against this user's Hermes API
// server, creating the Secret on first use. The Secret's INFERENCE_KEY (the
// user's pat-service PAT, ADR 0014 section 8) is written by
// scripts/hermes-inference-key until pat-service can mint it; a key rotation
// here merges into the Secret so INFERENCE_KEY survives.
func (b *PodsBackend) credential(ctx context.Context, id string) (string, error) {
	var sec struct {
		Data map[string]string `json:"data"`
	}
	err := b.k.do(ctx, http.MethodGet, b.k.path("secrets", secretName(id)), "", nil, &sec)
	if err == nil {
		raw, derr := base64.StdEncoding.DecodeString(sec.Data["API_SERVER_KEY"])
		if derr == nil && len(raw) >= 16 {
			return string(raw), nil
		}
	} else if !errors.Is(err, errNotFound) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	key := hex.EncodeToString(buf)
	obj := map[string]any{
		"apiVersion": "v1", "kind": "Secret", "type": "Opaque",
		"metadata":   map[string]any{"name": secretName(id), "labels": runtimeLabels(id)},
		"stringData": map[string]string{"API_SERVER_KEY": key},
	}
	method, p, ct := http.MethodPost, b.k.path("secrets", ""), "application/json"
	if err == nil {
		method, p, ct = http.MethodPatch, b.k.path("secrets", secretName(id)), "application/merge-patch+json"
	}
	if err := b.k.do(ctx, method, p, ct, obj, nil); err != nil {
		return "", err
	}
	return key, nil
}

type podObj struct {
	Metadata struct {
		Name              string            `json:"name"`
		Labels            map[string]string `json:"labels"`
		Annotations       map[string]string `json:"annotations"`
		DeletionTimestamp *string           `json:"deletionTimestamp"`
		CreationTimestamp time.Time         `json:"creationTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"`
		PodIP      string `json:"podIP"`
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		InitContainerStatuses []struct {
			Name  string `json:"name"`
			State struct {
				Terminated *struct {
					ExitCode int    `json:"exitCode"`
					Message  string `json:"message"`
				} `json:"terminated"`
			} `json:"state"`
		} `json:"initContainerStatuses"`
		ContainerStatuses []struct {
			RestartCount int `json:"restartCount"`
			State        struct {
				Waiting *struct {
					Reason  string `json:"reason"`
					Message string `json:"message"`
				} `json:"waiting"`
			} `json:"state"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

func (p *podObj) ready() bool {
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			return c.Status == "True"
		}
	}
	return false
}

func (p *podObj) report() *SyncReport {
	for _, s := range p.Status.InitContainerStatuses {
		if s.Name == syncContainer && s.State.Terminated != nil && s.State.Terminated.ExitCode == 0 {
			var r SyncReport
			if json.Unmarshal([]byte(s.State.Terminated.Message), &r) == nil && r.Version != "" {
				return &r
			}
		}
	}
	return nil
}

// failure returns a terminal reason for a pod that will not become ready.
func (p *podObj) failure() string {
	if p.Status.Phase == "Failed" {
		return "pod failed"
	}
	for _, s := range p.Status.InitContainerStatuses {
		if s.Name == syncContainer && s.State.Terminated != nil && s.State.Terminated.ExitCode != 0 {
			return "catalog sync failed: " + s.State.Terminated.Message
		}
	}
	for _, s := range p.Status.ContainerStatuses {
		if w := s.State.Waiting; w != nil {
			switch w.Reason {
			case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError":
				return w.Reason + ": " + w.Message
			case "CrashLoopBackOff":
				return "hermes is crash-looping"
			}
		}
	}
	return ""
}

func (b *PodsBackend) toAgent(p *podObj, key string) Agent {
	a := Agent{
		UserID:       p.Metadata.Labels[labelUser],
		APIKey:       key,
		Ready:        p.ready() && p.Metadata.DeletionTimestamp == nil,
		CatalogImage: p.Metadata.Annotations[annCatalog],
		StartedAt:    p.Metadata.CreationTimestamp,
		Report:       p.report(),
	}
	if p.Status.PodIP != "" {
		a.Endpoint = fmt.Sprintf("http://%s:%d", p.Status.PodIP, AgentPort)
	}
	if t, err := time.Parse(time.RFC3339, p.Metadata.Annotations[annActivity]); err == nil {
		a.LastActivity = t
	}
	return a
}

func (b *PodsBackend) getPod(ctx context.Context, id string) (*podObj, error) {
	var p podObj
	if err := b.k.do(ctx, http.MethodGet, b.k.path("pods", podName(id)), "", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

func (b *PodsBackend) Start(ctx context.Context, u User, spec StartSpec) (*Agent, error) {
	key, err := b.credential(ctx, u.ID)
	if err != nil {
		return nil, fmt.Errorf("credential: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, b.cfg.StartTimeout)
	defer cancel()
	created := false
	for {
		p, err := b.getPod(ctx, u.ID)
		switch {
		case errors.Is(err, errNotFound) && !created:
			// One writer per state.db: a terminating predecessor is gone
			// (404) before this create is attempted.
			if err := b.createPod(ctx, u, spec); err != nil && !strings.Contains(err.Error(), ": 409 ") {
				return nil, fmt.Errorf("create pod: %w", err)
			}
			created = true
		case err != nil && !errors.Is(err, errNotFound):
			return nil, err
		case p != nil && p.Metadata.DeletionTimestamp != nil:
			// Previous activation still shutting down; wait for it.
		case p != nil && p.ready():
			a := b.toAgent(p, key)
			if a.Report != nil {
				raw, _ := json.Marshal(a.Report)
				// Durable copy for /skills while no agent is running.
				_ = b.k.patchAnnotations(ctx, "persistentvolumeclaims", pvcName(u.ID), map[string]string{annReport: string(raw)})
			}
			return &a, nil
		case p != nil:
			if reason := p.failure(); reason != "" {
				return nil, errors.New(reason)
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("agent did not become ready: %w", ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (b *PodsBackend) createPod(ctx context.Context, u User, spec StartSpec) error {
	ann := map[string]string{
		annCatalog:  spec.CatalogImage,
		annActivity: time.Now().UTC().Format(time.RFC3339),
		annUsername: u.Name,
	}
	if spec.Selection != nil {
		raw, _ := json.Marshal(spec.Selection)
		ann[annSelection] = string(raw)
	}
	labels := runtimeLabels(u.ID)
	labels[labelComponent] = "hermes-agent"

	dropAll := map[string]any{"drop": []string{"ALL"}}
	sc := func(uid int) map[string]any {
		return map[string]any{
			"runAsUser": uid, "runAsGroup": 10000, "runAsNonRoot": true,
			"readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false,
			"capabilities": dropAll,
		}
	}
	env := []map[string]any{
		{"name": "HERMES_HOME", "value": "/opt/data/home"},
		{"name": "HOME", "value": "/work"},
		{"name": "TMPDIR", "value": "/tmp"},
		{"name": "API_SERVER_ENABLED", "value": "true"},
		{"name": "API_SERVER_HOST", "value": "0.0.0.0"},
		{"name": "API_SERVER_PORT", "value": fmt.Sprint(AgentPort)},
		{"name": "API_SERVER_MODEL_NAME", "value": "hermes-agent"},
		{"name": "API_SERVER_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]string{
			"name": secretName(u.ID), "key": "API_SERVER_KEY"}}},
		// config.yaml model.api_key reads it; absent = no inference.
		{"name": "HERMES_INFERENCE_KEY", "valueFrom": map[string]any{"secretKeyRef": map[string]any{
			"name": secretName(u.ID), "key": "INFERENCE_KEY", "optional": true}}},
		// The catalog is the only skill source: point Hermes' bundled-skill
		// seeding at a path that does not exist so it copies nothing.
		{"name": "HERMES_BUNDLED_SKILLS", "value": "/nonexistent"},
		{"name": "HERMES_OPTIONAL_SKILLS", "value": "/nonexistent"},
	}
	for k, v := range b.cfg.ExtraAgentEnv {
		env = append(env, map[string]any{"name": k, "value": v})
	}
	podSpec := map[string]any{
		"automountServiceAccountToken":  false,
		"enableServiceLinks":            false,
		"restartPolicy":                 "Always",
		"terminationGracePeriodSeconds": b.cfg.StopGrace,
		"securityContext": map[string]any{
			"runAsNonRoot": true, "fsGroup": 10000,
			"seccompProfile": map[string]string{"type": "RuntimeDefault"},
		},
		"initContainers": []map[string]any{{
			"name":            syncContainer,
			"image":           spec.CatalogImage,
			"imagePullPolicy": "IfNotPresent",
			"args":            []string{"sync"},
			"env": []map[string]any{
				{"name": "HERMES_HOME", "value": "/opt/data/home"},
				{"name": "HERMES_SELECTION", "valueFrom": map[string]any{"fieldRef": map[string]string{
					"fieldPath": "metadata.annotations['" + annSelection + "']"}}},
			},
			"securityContext": sc(10001),
			"resources": map[string]any{
				"requests": map[string]string{"cpu": "50m", "memory": "64Mi"},
				"limits":   map[string]string{"cpu": "500m", "memory": "128Mi"},
			},
			"volumeMounts": []map[string]any{{"name": "profile", "mountPath": "/opt/data"}},
		}},
		"containers": []map[string]any{{
			"name":            "hermes",
			"image":           b.cfg.HermesImage,
			"imagePullPolicy": "IfNotPresent",
			// Hermes directly, without the image's s6 /init: that bootstrap
			// needs root, and the catalog sync has already prepared the
			// profile. umask 002 keeps personal files group-writable for the
			// sync uid (it archives shadowed personal skills).
			"command": []string{"/bin/sh", "-c", "umask 002 && exec /opt/hermes/.venv/bin/hermes gateway run"},
			"env":     env,
			"ports":   []map[string]any{{"name": "api", "containerPort": AgentPort}},
			"readinessProbe": map[string]any{
				"httpGet":       map[string]any{"path": "/health", "port": "api"},
				"periodSeconds": 1, "failureThreshold": 3,
			},
			"livenessProbe": map[string]any{
				"httpGet":             map[string]any{"path": "/health", "port": "api"},
				"initialDelaySeconds": 60, "periodSeconds": 20, "failureThreshold": 3,
			},
			"securityContext": sc(10000),
			"resources": map[string]any{
				"requests": map[string]string{"cpu": b.cfg.CPURequest, "memory": b.cfg.MemRequest},
				"limits":   map[string]string{"cpu": b.cfg.CPULimit, "memory": b.cfg.MemLimit},
			},
			"volumeMounts": []map[string]any{
				{"name": "profile", "mountPath": "/opt/data"},
				{"name": "work", "mountPath": "/work"},
				{"name": "tmp", "mountPath": "/tmp"},
			},
		}},
		"volumes": []map[string]any{
			{"name": "profile", "persistentVolumeClaim": map[string]string{"claimName": pvcName(u.ID)}},
			{"name": "work", "emptyDir": map[string]string{"sizeLimit": b.cfg.WorkSize}},
			{"name": "tmp", "emptyDir": map[string]string{"sizeLimit": "256Mi"}},
		},
	}
	if b.cfg.RuntimeClass != "" {
		podSpec["runtimeClassName"] = b.cfg.RuntimeClass
	}
	pod := map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{"name": podName(u.ID), "labels": labels, "annotations": ann},
		"spec":     podSpec,
	}
	return b.k.do(ctx, http.MethodPost, b.k.path("pods", ""), "application/json", pod, nil)
}

func (b *PodsBackend) Stop(ctx context.Context, id string) error {
	body := map[string]any{"gracePeriodSeconds": b.cfg.StopGrace}
	err := b.k.do(ctx, http.MethodDelete, b.k.path("pods", podName(id)), "application/json", body, nil)
	if err != nil && !errors.Is(err, errNotFound) {
		return err
	}
	deadline := time.Now().Add(time.Duration(b.cfg.StopGrace)*time.Second + 30*time.Second)
	for time.Now().Before(deadline) {
		if _, err := b.getPod(ctx, id); errors.Is(err, errNotFound) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("pod %s still present after delete", podName(id))
}

func (b *PodsBackend) List(ctx context.Context) ([]Agent, error) {
	var list struct {
		Items []podObj `json:"items"`
	}
	q := "?labelSelector=" + url.QueryEscape(labelComponent+"=hermes-agent")
	if err := b.k.do(ctx, http.MethodGet, b.k.path("pods", "")+q, "", nil, &list); err != nil {
		return nil, err
	}
	out := make([]Agent, 0, len(list.Items))
	for i := range list.Items {
		out = append(out, b.toAgent(&list.Items[i], ""))
	}
	return out, nil
}

func (b *PodsBackend) Touch(ctx context.Context, id string, at time.Time) error {
	return b.k.patchAnnotations(ctx, "pods", podName(id), map[string]string{annActivity: at.UTC().Format(time.RFC3339)})
}

func (b *PodsBackend) Profile(ctx context.Context, id string) (*SyncReport, error) {
	var pvc struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := b.k.do(ctx, http.MethodGet, b.k.path("persistentvolumeclaims", pvcName(id)), "", nil, &pvc); err != nil {
		if errors.Is(err, errNotFound) {
			return nil, nil
		}
		return nil, err
	}
	raw := pvc.Metadata.Annotations[annReport]
	if raw == "" {
		return nil, nil
	}
	var r SyncReport
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, nil
	}
	return &r, nil
}
