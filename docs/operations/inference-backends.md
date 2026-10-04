# Inference backends and model routing

How to bring up an additional backend and route to it by model name. The
decision behind this shape is in
[ADR 0006](../adr/0006-pluggable-inference-backends.md), and how the engines
themselves are deployed in
[ADR 0007](../adr/0007-inference-engines-as-helm-releases.md); vLLM's own
launch settings are in [vllm-inference.md](vllm-inference.md).

## What is available

| Backend | Model name | Runs as | Reached via | Route/engine chosen by |
| --- | --- | --- | --- | --- |
| vLLM | `qwen-3.8-27b` (pool member) | `helm/vllm-inference` release, `make vllm-up` | llm-d EPP | `VLLM_REPLICAS`, `make engines-up` |
| SGLang | `qwen-3.8-27b` (pool member) | `helm/sglang-inference` release, `make sglang-up` | llm-d EPP | `SGLANG_REPLICAS`, `make engines-up` |
| ninfer (pilot) | `qwen-3.8-27b-ninfer` (its own) | `helm/ninfer-inference` release, `make ninfer-up` | direct `AIServiceBackend`, bypasses EPP | `NINFER_REPLICAS`, `make engines-up` |
| llama.cpp | `llamacpp-local` | `deploy/llamacpp` (host Docker) | direct `AIServiceBackend`, bypasses EPP | `inference.llamacpp.enabled` |
| External API | `external-api` (or whatever `inference.externalApi.modelName` is set to) | not managed by this repo; here: Strata on the GPU host, `qwen-3.8-flash-next` (`deploy/strata`) | direct `AIServiceBackend`, bypasses EPP | `inference.externalApi.enabled` |

The canonical model name, `qwen-3.8-27b`, is a pool
([ADR 0019](../adr/0019-inference-plane-gpu-worker-nodes.md)). The
exact-match rule on `AIGatewayRoute/llmd` (`openai-qwen38nvfp4`) and the
catch-all `openai` always forward to `llmd-qwen-test-openai`, llm-d EPP's own
backend, never to an engine directly. EPP dispatches to every pod labelled
`llm-d.ai/model=qwen-3.8-27b` on port 8000
(`config/llmd/router-nvfp4-values.yaml` `router.modelServers`), vLLM and
SGLang alike, so llm-d's queue, priority bands and fair share cover every
member ([ADR 0008](../adr/0008-per-user-fair-share.md)). Which engines serve
is their replica counts, not a routing change. The engines run without an
API key; the NetworkPolicy `inference-pool-members` admits only EPP and
Prometheus to them.

ninfer cannot join EPP (see "EPP / fair-share" in `deploy/ninfer/README.md`
-- EPP's scoring plugins need a `/metrics` endpoint ninfer does not have), so
it is its own model name, `qwen-3.8-27b-ninfer`
(`inference.ninfer.modelName`, the chart's `--model-id`), on its own rule
`openai-ninfer` to `ninfer-openai`, with `ninfer-api-key` injected by a
`BackendSecurityPolicy`. Its traffic has none of ADR-0008/ADR-0012's
fair-share or priority-band guarantees, and ninfer's own capability gaps
apply (no JSON-mode `response_format`, see `deploy/ninfer/README.md`
"Compatibility gaps"). `inference.ninfer.enabled` gates its
`Backend`/`AIServiceBackend`, Secret and rule.

llama.cpp and the external API follow the same pattern as ninfer: their own
`Backend`/`AIServiceBackend` pair and their own exact-match rule on a
*different* model name, toggled by `inference.<name>.enabled` in
`helm/airgap-stack/values.yaml` (see `helm/airgap-stack/templates/
inference-backends.yaml`). Traffic to those model names bypasses llm-d
entirely — no queue, no fair-share, no priority bands. That is an accepted,
documented gap (`TASK-qos-fair-share.md` §7.6).

A client selects the model by request `model`; Open WebUI's model dropdown,
the `/v1/models` endpoint, and `x-ai-eg-model` all key off the same name.
Every backend runs behind the one private AI Gateway route
(`AIGatewayRoute/llmd`), so the same JWT check and per-user rate limit
already covering vLLM cover every backend added this way — there is nothing
extra to configure per backend for auth.

## Single GPU, one engine at a time

This host has one RTX 5090. All three in-cluster engines (vLLM, SGLang,
ninfer) request and limit `nvidia.com/gpu: 1` with `strategy: Recreate`, so
the scheduler arbitrates: start a second one and its pod sits `Pending` with
`insufficient nvidia.com/gpu` instead of fighting for the card.

Capacity is replicas per engine in `.env`, applied in GPU-safe order by one
command:

```sh
# .env: VLLM_REPLICAS=0 SGLANG_REPLICAS=1 NINFER_REPLICAS=0
make engines-up
```

`engines-up` stops GPU embeddings and the engines going to 0, starts the
others and waits for them, runs `make llmd-up`, and starts embeddings again
last. EPP reads each engine's metric names from the pod label
`llm-d.ai/engine-type` set by the engine charts. With one GPU keep the total
at 1; moving it to ninfer leaves `qwen-3.8-27b` with no endpoints. Background
and checks: [docs/handbook/engines](../handbook/engines/README.md).

`make vllm-down` / `make sglang-down` / `make ninfer-down` uninstall a release
outright, which is the right move when the engine is not coming back soon;
`engines-up` scales an engine to zero and keeps its release.

ninfer reads its upstream API key from the `ninfer-api-key` Secret, created by
the `airgap-stack` release from `NINFER_API_KEY`; the same Secret backs the
`BackendSecurityPolicy` on `ninfer-openai`.

llama.cpp still runs as a host Docker container outside Kubernetes' view and
defaults to `N_GPU_LAYERS=0` (CPU-only) for that reason; only raise it while
no in-cluster engine is running.

## Turning on a backend at the gateway

This section is about llama.cpp and the external API — the two backends that
still get their own `AIGatewayRoute` rule and model name (see "What is
available" above). vLLM and SGLang never add or remove a route rule;
verifying a replica change is the EPP check at the end of this section, not
an `aigatewayroute` diff.

Enabling `inference.<name>.enabled` only tells the gateway the endpoint now
exists — it never starts the engine. Start it first (`deploy/llamacpp`, or
point `externalApi` at an endpoint you already run), confirm its own smoke
test passes, then, for a backend whose route is not already enabled in
`values.yaml`:

```sh
helm upgrade --install airgap-stack helm/airgap-stack \
  --namespace airgap-ai-stack --values helm/airgap-stack/values.yaml \
  --set inference.llamacpp.enabled=true
```

`make helm-up` layers `.env` the same way it already does for the rest of
`runtimeEnvironment`, so setting `EXTERNAL_API_KEY` there
and flipping `inference.*.enabled` in `values.yaml` is the tracked way to do
it. Open WebUI's model dropdown is a separate list —
`OPENAI_API_CONFIGS` in `k8s/overlays/remote-wsl-vllm-nvfp4/openwebui-oidc-patch.yaml`
— because llm-d's fixed endpoint does not expose a catalogue to its discovery
client; add or remove the model id there in the same change. After the
upgrade, confirm the route picked it up:

```sh
kubectl -n airgap-ai-stack get aigatewayroute llmd -o yaml   # new rule present
curl -s $STACK_BASE_URL/v1/models -H "Authorization: Bearer sk-…" \
  | grep qwen-3.8-27b
```

After a vLLM/SGLang replica change, confirm EPP sees the pool members
(`llm_d_epp_ready_endpoints` equals the replicas):

```sh
kubectl -n airgap-ai-stack get pods -l llm-d.ai/model=qwen-3.8-27b -L llm-d.ai/engine-type
kubectl -n airgap-ai-stack logs deployment/llmd-qwen-test-epp | tail -50   # endpoint picked, no dial errors
curl -s $STACK_BASE_URL/v1/chat/completions -H "Authorization: Bearer sk-…" \
  -d '{"model":"qwen-3.8-27b","messages":[{"role":"user","content":"hi"}],"max_tokens":8}'
```

## Reachability from the cluster

In-cluster engines are addressed over Service DNS: vLLM at
`vllm-qwen38-nvfp4:8000`, SGLang at `sglang-qwen38:8000` (both only from EPP
and Prometheus), ninfer at
`ninfer-qwen38:18080` (`inference.ninfer.host`/`port` in
`helm/airgap-stack/values.yaml` — unlike llama.cpp, ninfer is always an
in-cluster Deployment, never a host-run container; see
`deploy/ninfer/README.md` Risk 1). Prometheus scrapes vLLM, SGLang and the
llm-d EPP directly, plus a sidecar that re-exposes ninfer's JSONL request log
as Prometheus text (ninfer has no native `/metrics`) under `job: ninfer`, all
with `namespace=airgap-ai-stack` attached
(`k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`).

A host-run backend (llama.cpp, or SGLang started from
`deploy/sglang-qwen38` instead) is addressed as `host.k3d.internal:<port>` —
k3d's own DNS alias for the WSL2 host's Docker bridge gateway, present on
every cluster k3d creates, not something this site had to configure. Verify
it from inside the node:

```sh
docker exec k3d-llm-stack-server-0 sh -c \
  'wget -qO- --timeout=8 http://host.k3d.internal:<port>/v1/models'
```
