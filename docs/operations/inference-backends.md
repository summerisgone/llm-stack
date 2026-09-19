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
| vLLM | `qwen-3.8-27b` | `helm/vllm-inference` release, `make vllm-up` | llm-d EPP | `config/llmd/router-nvfp4-values.yaml` `router.modelServers`, `inference.ninfer.live: false` |
| SGLang | `qwen-3.8-27b` | `helm/sglang-inference` release, `make sglang-up` | llm-d EPP | `config/llmd/router-nvfp4-values.yaml` `router.modelServers`, `inference.ninfer.live: false` |
| ninfer (pilot) | `qwen-3.8-27b` (shared, not its own) | `helm/ninfer-inference` release, `make ninfer-up` | direct `AIServiceBackend`, bypasses EPP | `inference.ninfer.live: true` |
| llama.cpp | `llamacpp-local` | `deploy/llamacpp` (host Docker) | direct `AIServiceBackend`, bypasses EPP | `inference.llamacpp.enabled` |
| External API | `external-api` (or whatever `inference.externalApi.modelName` is set to) | not managed by this repo | direct `AIServiceBackend`, bypasses EPP | `inference.externalApi.enabled` |

The canonical vLLM/SGLang model name, `qwen-3.8-27b`, is shared by up to
three engines now, not two. Both `helm/vllm-inference` and
`helm/sglang-inference` run with `--served-model-name qwen-3.8-27b`, and
`helm/ninfer-inference` with `--model-id qwen-3.8-27b`
(`inference.servedModelName`) — the exact-match rule on `AIGatewayRoute/llmd`
(`openai-qwen38nvfp4`) matches that one name for all three, but its
`backendRefs` target is not fixed the same way for all three:

- **vLLM <-> SGLang**: the rule always forwards to `llmd-qwen-test-openai`,
  llm-d EPP's own backend. It never points at either engine directly, so
  switching between them is never a gateway routing change: it is entirely a
  fact about which engine is running and which one
  `config/llmd/router-nvfp4-values.yaml`'s `router.modelServers` block
  (`type`, `targetPorts`, `matchLabels`) tells EPP to dispatch to. llm-d's
  queue, priority bands and fair-share dispatch (the whole point of
  `TASK-qos-fair-share.md`) cover both engines identically — see
  [ADR 0008](../adr/0008-per-user-fair-share.md) "SGLang portability" for why
  this replaced an earlier design where SGLang had its own direct
  `AIServiceBackend` that bypassed EPP whenever `inference.sglang.enabled`
  was set (§7.1 of the task doc).
- **ninfer**: cannot join EPP itself (see "EPP / fair-share" in
  `deploy/ninfer/README.md` — EPP's scoring plugins need a `/metrics`
  endpoint ninfer does not have, not just an unverified engine-type string).
  `inference.ninfer.live` in `helm/airgap-stack/values.yaml` instead
  retargets this *same* rule's `backendRefs` directly to `ninfer-openai`
  when `true`, or back to `llmd-qwen-test-openai` when `false`
  (`helm/airgap-stack/templates/llmd.yaml`). A client's `model` field still
  never changes, but while `live: true`, that traffic has none of
  ADR-0008/ADR-0012's fair-share or priority-band guarantees, and ninfer's
  own capability gaps apply (no JSON-mode `response_format`, no constrained
  decoding — see `deploy/ninfer/README.md` "Compatibility gaps").

`inference.sglang.enabled` in `helm/airgap-stack/values.yaml` still exists,
but only to gate injecting `SGLANG_API_KEY` (when set) as the upstream
`Authorization` header on the shared EPP path — SGLang requires a bearer
token, vLLM does not, and EPP's dispatch path has no per-endpoint auth hook
of its own (`helm/airgap-stack/templates/llmd.yaml`, `BackendSecurityPolicy/
sglang-api-key`). It plays no part in choosing which engine answers.
`inference.ninfer.enabled` is the equivalent for ninfer's own
`BackendSecurityPolicy`/Secret, but unlike `sglang.enabled` it also gates
whether ninfer's `Backend`/`AIServiceBackend` exist at all — `live` is the
separate flag that actually routes traffic to it (see
`helm/airgap-stack/templates/inference-backends.yaml`'s ninfer comment).

llama.cpp and the external API stay on the older, simpler pattern ninfer
used to use before it moved onto the canonical name above: their own
`Backend`/`AIServiceBackend` pair and their own exact-match rule on a
*different* model name, toggled by `inference.<name>.enabled` in
`helm/airgap-stack/values.yaml` (see `helm/airgap-stack/templates/
inference-backends.yaml`). Traffic to those model names bypasses llm-d
entirely — no queue, no fair-share, no priority bands. That is an accepted,
documented gap (`TASK-qos-fair-share.md` §7.6), not a bug: neither is (yet)
reached by real coding-agent traffic under this scheme, unlike ninfer, which
now can be by design.

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

Since [ADR 0008](../adr/0008-per-user-fair-share.md) "SGLang portability",
switching the live engine is **three steps, not one** — scale the engines,
point llm-d EPP at the new one, and (SGLang only) make sure its upstream key
is wired up. Skipping the EPP step leaves it polling a `Pending` pod's
`/metrics` and dispatching into a black hole.

```sh
# vLLM -> SGLang
kubectl -n airgap-ai-stack scale deployment/vllm-qwen38-nvfp4 --replicas=0
make sglang-up
# helm/airgap-stack/values.yaml: inference.sglang.enabled: true, and
# SGLANG_API_KEY set in .env, if SGLang requires one -- then:
make helm-up
# config/llmd/router-nvfp4-values.yaml: router.modelServers.type: sglang,
# targetPorts: [30000], matchLabels app.kubernetes.io/name: sglang-qwen38;
# core-metrics-extractor.defaultEngine: sglang; concurrency-detector.
# maxConcurrency matching SGLang's --max-running-requests -- then:
make llmd-up

# SGLang -> vLLM
kubectl -n airgap-ai-stack scale deployment/sglang-qwen38 --replicas=0
kubectl -n airgap-ai-stack scale deployment/vllm-qwen38-nvfp4 --replicas=1
# config/llmd/router-nvfp4-values.yaml: router.modelServers.type: vllm,
# targetPorts: [8000], matchLabels app.kubernetes.io/name: vllm-qwen38-nvfp4;
# drop core-metrics-extractor.defaultEngine; concurrency-detector.
# maxConcurrency matching vLLM's --max-num-seqs -- then:
make llmd-up
# helm/airgap-stack/values.yaml: inference.sglang.enabled: false, then:
make helm-up

# (vLLM or SGLang) -> ninfer: no EPP step, since ninfer never joins EPP --
# retargeting the shared route rule directly is the whole swap.
kubectl -n airgap-ai-stack scale deployment/vllm-qwen38-nvfp4 --replicas=0
kubectl -n airgap-ai-stack scale deployment/sglang-qwen38 --replicas=0
make ninfer-up
# helm/airgap-stack/values.yaml: inference.ninfer.enabled: true,
# inference.ninfer.live: true, and NINFER_API_KEY set in .env -- then:
make helm-up

# ninfer -> vLLM (or SGLang): reverse order, live: false first so the route
# stops pointing at a pod you're about to scale down.
# helm/airgap-stack/values.yaml: inference.ninfer.live: false, then:
make helm-up
kubectl -n airgap-ai-stack scale deployment/vllm-qwen38-nvfp4 --replicas=1
# (or make sglang-up / scale sglang-qwen38, per the vLLM<->SGLang steps above)
```

`make vllm-down` / `make sglang-down` uninstall the release outright, which is
the right move when the engine is not coming back soon; scaling to zero keeps
the release and its values in place.

SGLang reads its upstream API key from the `sglang-api-key` Secret, created by
the `airgap-stack` release when both `inference.sglang.enabled` and
`SGLANG_API_KEY` are set. Run `make helm-up` with those set before `make
sglang-up`, or the pod cannot start; the same Secret also backs the
`BackendSecurityPolicy` that injects that key into the shared EPP path
(`helm/airgap-stack/templates/llmd.yaml`), so a stale or missing key means
401s from SGLang through the normal `qwen-3.8-27b` route, not just a failed
pod start.

llama.cpp still runs as a host Docker container outside Kubernetes' view and
defaults to `N_GPU_LAYERS=0` (CPU-only) for that reason; only raise it while
neither vLLM nor SGLang is running.

## Turning on a backend at the gateway

This section is about llama.cpp and the external API — the two backends that
still get their own `AIGatewayRoute` rule and model name (see "What is
available" above). SGLang never adds or removes a route rule; verifying an
engine switch to or from SGLang is the EPP check at the end of this section,
not an `aigatewayroute` diff.

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
`runtimeEnvironment`, so setting `SGLANG_API_KEY` / `EXTERNAL_API_KEY` there
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

After a vLLM/SGLang switch, confirm EPP actually picked up the new engine
rather than continuing to poll a scaled-down pod's now-`Pending` `/metrics`:

```sh
kubectl -n airgap-ai-stack get pods -l 'app.kubernetes.io/name in (vllm-qwen38-nvfp4,sglang-qwen38)'
kubectl -n airgap-ai-stack logs deployment/llmd-qwen-test-epp | tail -50   # endpoint picked, no dial errors
curl -s $STACK_BASE_URL/v1/chat/completions -H "Authorization: Bearer sk-…" \
  -d '{"model":"qwen-3.8-27b","messages":[{"role":"user","content":"hi"}],"max_tokens":8}'
```

## Reachability from the cluster

In-cluster engines are addressed over Service DNS: vLLM at
`vllm-qwen38-nvfp4:8000`, SGLang at `sglang-qwen38:30000`, ninfer at
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
