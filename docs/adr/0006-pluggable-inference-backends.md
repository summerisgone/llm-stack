# 0006: Pluggable inference backends, selected by model name

## Status

Accepted.

## Context

The only serving path was vLLM behind the llm-d EPP
(`k8s/overlays/remote-wsl-vllm-nvfp4/vllm.yaml`,
`helm/airgap-stack/templates/llmd.yaml`). Two other engines are already
operated on the same remote host outside this repo: SGLang, run as a Docker
container directly on the WSL2 host (`/home/llmstack/sglang-qwen38` on
`gpu-host.local`), and an external OpenAI-compatible endpoint on the LAN
(LM Studio, wired for the `local-mac` profile in
`k8s/overlays/local-mac/lmstudio.yaml`). llama.cpp's server has no wiring at
all yet. There was no way to add a backend without either replacing the
active route or hand-editing cluster objects outside Git.

The host has exactly one GPU (RTX 5090). vLLM's Deployment already requests
`nvidia.com/gpu: 1` with `strategy: Recreate` for single-GPU exclusivity
(see [docs/architecture](../architecture/README.md#inference-profile)).
SGLang's own Docker invocations use `--gpus all` outside Kubernetes' view, so
the two can silently compete for the same card if both are started; nothing
in the cluster can arbitrate that, because the cluster only schedules one of
them.

## Decision

Model an inference backend as: one Envoy Gateway `Backend` (its network
endpoint), one `AIServiceBackend` (OpenAI schema), and one exact-match rule
on `AIGatewayRoute/llmd` keyed on the `x-ai-eg-model` header — the same
mechanism already used to give vLLM's `qwen38-nvfp4` model its own rule
alongside the route's catch-all. Adding a backend is adding a rule, not
replacing one; a client selects it by request `model` name, same as Open
WebUI's model dropdown already does.

Three backends are added this way, all in
`helm/airgap-stack/templates/inference-backends.yaml` (all `AIGatewayRoute`,
`Backend` and `AIServiceBackend` objects are chart-owned per
[ADR 0002](0002-one-owner-per-object.md); `scripts/helm-render` already fails
the build if one leaks into the kustomize overlay instead), each off by
default behind `.Values.inference.<name>.enabled`:

- **SGLang** (`inference.sglang`) — a host-managed Docker container
  (`deploy/sglang-qwen38/run`, mirroring `deploy/vllm-qwen38-nvfp4`),
  reached from the cluster at `host.k3d.internal:30000`. `host.k3d.internal`
  is k3d's own DNS name for the WSL2 host's Docker bridge gateway
  (`172.21.0.1` on this cluster); nothing site-specific needs to be hardcoded
  because k3d provides it in every cluster it creates. Verified from inside
  `k3d-llm-stack-server-0` against the host's running SGLang container
  before this was written.
- **llama.cpp** (`inference.llamacpp`) — the same pattern
  (`deploy/llamacpp/run`), reached at `host.k3d.internal:8090`. No model is
  wired up yet (`config/llamacpp` stays a profile template until a
  checksummed GGUF is in place), so this stays disabled until an operator
  fills one in.
- **External OpenAI-compatible API** (`inference.externalApi`) — the same
  role LM Studio already plays for `local-mac`, generalized: any host/port an
  operator points it at (LM Studio, Ollama, a hosted API reachable in plain
  HTTP). Host is empty by default; enabling it without setting a host is a
  configuration error the operator must fix, not a default that silently
  serves nothing.

Optional bearer-token upstream auth (SGLang and the external API can both
require one) uses `BackendSecurityPolicy` (`type: APIKey`), Envoy AI
Gateway's own mechanism for injecting a static `Authorization` header,
sourced from a dedicated Secret populated from `runtimeEnvironment` — not
reused from the app-facing `airgap-runtime` Secret, since the API key must
live under the literal key `apiKey` that `BackendSecurityPolicy` requires.

## Consequences

- Turning on a second GPU backend is an operational decision the chart
  cannot make safely: enabling `inference.sglang` does not stop vLLM, and
  running both at once on one GPU will starve or crash one of them. This is
  documented next to the values, in
  `config/sglang/model-profiles/README.md`, and in
  [docs/operations](../operations/README.md); the chart does not attempt to
  enforce it in software, because the two engines are started and stopped
  outside Kubernetes entirely.
- `AIGatewayRoute` allows at most 15 rules including a reserved catch-all
  slot; the route now uses 5. Additional backends are cheap until that
  budget runs out, at which point they split across a second
  `AIGatewayRoute` on the same Gateway.
- `local-mac`'s LM Studio wiring is untouched: it is the same pattern this
  ADR generalizes, not a duplicate of it, so it stays where
  [ADR 0002](0002-one-owner-per-object.md) put it.
