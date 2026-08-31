# Operations — remote host access

Day-2 procedures for a running installation. Standing one up from nothing is
[docs/install](../install/README.md).

Everything below — the SSH coordinates, ports, node name, paths and operator
UI hostnames — describes the **reference site**. On another installation these
are the values to substitute; the inventory of what is site-specific is in
[docs/install](../install/README.md).

The reference remote RTX 5090 profile lives on a single Windows + WSL2 host:

    llmstack@gpu-host.local -p2222

SSH works even though ICMP ping is blocked (`ping gpu-host.local` shows 100%
loss, but `ssh` connects). The WSL2 user home is `/home/llmstack`, so model
paths look like `/home/llmstack/models/...`. Model dir is bind-mounted
read-only into the k3d node at `/var/lib/models`.

## Running `kubectl` from the local machine

The local kubeconfig context `wsl-llm-stack` points the API server at
`https://127.0.0.1:41755`. Forward the remote API port to localhost with an
SSH tunnel, then `kubectl` works as if local:

    make k3s-tunnel

The tunnel runs in the foreground; leave it running and use a second terminal:

    kubectl --context wsl-llm-stack get nodes

Stop it with `Ctrl+C`. The listener binds only to `127.0.0.1`; SSH fails if
the local port is already occupied and uses keepalives to detect a lost
connection. For another host or API port, override the defaults:

    make k3s-tunnel WSL_SSH_HOST=user@host WSL_SSH_PORT=22 K3S_LOCAL_PORT=16443 K3S_REMOTE_PORT=6443

The kubeconfig server URL must use the selected local port. This target does
not change kubeconfig or the current context.

## Edge (routes) tunnel for smoke tests

Smoke scripts hit the Envoy edge `:8080` as localhost. Forward it to a local
port and pass that port as `STACK_TUNNEL_PORT`:

    ssh -fN -L 18080:127.0.0.1:8080 llmstack@gpu-host.local -p2222
    STACK_TUNNEL_PORT=18080 make llmd-nvfp4-smoke

While the GPU is occupied by other work, the same coverage minus the calls
that reach the model:

    STACK_TUNNEL_PORT=18080 make smoke-nogpu

## Operator UIs

For the vLLM launch settings, measured throughput, and Helm-provisioned Grafana
dashboard, see [vLLM inference and monitoring](vllm-inference.md). For the
optional SGLang, llama.cpp and external-API backends and how requests are
routed to them by model name, see
[inference backends and model routing](inference-backends.md).

## Changing the model server

The engines are Helm releases of their own, so a launch-flag change restarts
one pod and nothing else:

    $EDITOR helm/vllm-inference/values.yaml
    make vllm-up                        # or make vllm-down to remove it

`make sglang-up` / `make sglang-down` do the same for SGLang, which is not
part of `make stack-up`. One GPU: bring one down before the other comes up.

## MinIO Operator

`make minio-operator-up` updates only the `minio-operator` Helm release.
`make operators-up` and `make stack-up` use the same target. Select the
intended Kubernetes context before running it.

The operator image is the [gitgrave v7.1.2 release](https://github.com/gitgrave/minio-operator/releases/tag/v7.1.2),
pinned by tag and multi-architecture digest in `versions.lock.env`. It includes
[upstream fix #2463](https://github.com/minio/operator/pull/2463): after losing
the Kubernetes leader lease, the operator exits instead of spinning on a
closed channel and consuming a CPU. The deployment still uses upstream Helm
chart 7.1.1 with an image override, so `helm list` reports chart/app version
7.1.1; inspect the Deployment image to confirm the running operator version.
The MinIO server and tenant sidecar images are independent of this override.

`config/minio-operator/values.yaml` disables client-go's `WatchListClient`
feature for this image. Its custom pod informer drops the requested
`ListOptions`, including `SendInitialEvents`, so the newly enabled streaming
list waits indefinitely for an initial-events bookmark. List-then-watch
preserves pod health monitoring and avoids repeated `event bookmark expired`
warnings. Keep this setting until that informer is fixed.

After an update, verify both replicas are ready, the lease `renewTime`
continues advancing for longer than its 60-second duration, and CPU returns
to idle:

```sh
kubectl -n minio-operator rollout status deployment/minio-operator --timeout=180s
kubectl -n minio-operator get lease minio-operator-lock -o yaml
kubectl -n minio-operator top pods
kubectl -n airgap-ai-stack get tenant minio
```

## Keycloak administration

`/sso/admin` and `/sso/realms/master` are refused at the edge. Reach the admin
console through a port-forward on the fixed port Keycloak's `KC_HOSTNAME_ADMIN`
is pinned to:

    kubectl -n airgap-ai-stack port-forward svc/keycloak 8888:8080
    # http://localhost:8888/sso/admin

The port matters: `KC_HOSTNAME_ADMIN` is an absolute URL Keycloak does not
derive from the request, so the console breaks on any other local port.

Grafana and Langfuse use dedicated NodePorts: `grafana-nodeport:32030` and
`langfuse-nodeport:32031`. New k3d clusters publish both ports directly on
WSL. For an existing cluster, install the persistent user-level WSL proxies:

```sh
deploy/vllm-qwen38-nvfp4/install-wsl-operator-ui-proxy
```

To make the WSL listeners reachable on the parent Windows host and its private
LAN, register a scheduled task once from an elevated Windows PowerShell
session (`wsl -l -q` lists your distro name):

```powershell
deploy\vllm-qwen38-nvfp4\install-windows-operator-ui-portproxy-task.ps1 -Distro <your WSL distribution>
```

This creates a task (`LLM Stack Operator UI Portproxy`) that resolves the
current WSL `eth0` address and reapplies idempotent `netsh interface
portproxy` entries and Windows Firewall rules limited to the private local
subnet — at boot, at logon, and every 5 minutes thereafter, so it self-heals
after the WSL NAT address changes (which happens on every WSL restart, not
just a host reboot) without any manual step. With DNS records for the
Windows host, the UIs are available at:

- `http://grafana.gpu-host.local:32030`
- `http://langfuse.gpu-host.local:32031`

To reapply immediately instead of waiting on the schedule: `Start-ScheduledTask
-TaskName 'LLM Stack Operator UI Portproxy'`, or run
`configure-windows-operator-ui-portproxy.ps1 -Distro <name>` directly.
Grafana's OIDC callback and Langfuse's public URL are configured for these
addresses.

## AI Gateway traces in Langfuse

The private AI Gateway samples all inference-path requests and sends OTLP to
the in-cluster Collector. The Collector authenticates to Langfuse using
`LANGFUSE_PUBLIC_KEY` and `LANGFUSE_SECRET_KEY` from `.env`; those credentials
are never exposed to Envoy.

HTTP tracing on `EnvoyProxy` supplies request timing only. The separate
`GatewayConfig/ai-gateway-tracing` enables GenAI tracing in AIGW's external
processor using OpenInference conventions. Langfuse maps `input.value`,
`output.value`, and `llm.model_name` to the standard input, output, and model
fields of a generation, with token usage from `llm.token_count.*`.
Request and response content capture is explicitly enabled, including for
streaming completions. This affects new requests only; old HTTP-only traces
cannot recover missing prompts or answers.
The Collector drops the `ai-gateway-private` Envoy transport spans before
exporting to Langfuse; only the separate `ai-gateway-genai` observations from
this gateway are retained. Existing HTTP spans are not deleted.

`config/ai-gateway/values.yaml` maps trusted identity headers onto the GenAI
spans too. Apply these controller settings with the `aieg` Helm release;
the per-gateway tracing settings belong to `airgap-stack`.

For PAT requests, the service persists the SSO `preferred_username` with each
new token and adds it to the trusted, internal request to the AI Gateway.
Langfuse uses this as `userId` and records the SSO subject and PAT ID as
trace metadata. Client-provided identity headers are stripped before PAT
validation. Tokens issued before this migration have no stored username and
fall back to the stable SSO subject; issue a replacement PAT to record the
username on later traces. Open WebUI maps its SSO identity to the same
internal headers, so browser-originated chats receive the same user context.

## After a WSL2 host reboot

The k3d server node often exits 128 during boot (cgroup race). The load
balancer (`k3d-llm-stack-serverlb`) starts first, but the API is dead until
the server node is restarted:

    ssh llmstack@gpu-host.local -p2222
    docker start k3d-llm-stack-server-0

Then re-establish the tunnels above and verify health:

    kubectl get nodes                       # server Ready
    kubectl -n airgap-ai-stack get pods     # database pods (pat-db-1, keycloak-db-1,
                                            #   langfuse-db-1) come up first; the apps
                                            #   (keycloak, pat-service, langfuse) restart
                                            #   until their DB is Ready — transient.
    kubectl -n airgap-ai-stack get pod -l app.kubernetes.io/name=vllm-qwen38-nvfp4
                                            # reloads the 27B checkpoint (~1-2 min)

All pods settle to Running after the databases are Ready (usually under a
minute); the vLLM pod takes longer because it reloads the model from disk.
