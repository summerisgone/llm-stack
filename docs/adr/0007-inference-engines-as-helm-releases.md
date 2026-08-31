# 0007: Inference engines are their own Helm releases, in-cluster

## Status

Accepted. Supersedes the *deployment mechanism* for SGLang described in
[ADR 0006](0006-pluggable-inference-backends.md); the model-name routing
decision in that ADR is unchanged and still how a backend is selected.

## Context

Two things were true after ADR 0006 and neither survived contact with tuning
work:

- The vLLM Deployment was a literal in
  `k8s/overlays/remote-wsl-vllm-nvfp4/vllm.yaml`. Every sizing experiment
  (`--max-model-len`, `--max-num-seqs`, prefix caching, the mamba cache
  flags) meant editing a manifest, re-rendering the wrapper chart and
  restarting the whole application release to change one flag on one pod.
  `helm/vllm-inference` already existed as a values-driven proposal but
  nothing installed it.
- SGLang ran as a host Docker container reached at
  `host.k3d.internal:30000`. That put the GPU consumer outside Kubernetes'
  view: the scheduler could not see the contention with vLLM, so starting
  both silently starved or crashed one of them, and the engine had no
  readiness probe, no Prometheus target inside the cluster, and no
  reproducible definition in Git beyond an env file.

Both engines mount the same read-only model PVC and need the same
`RuntimeClass/nvidia`. There is exactly one RTX 5090.

## Decision

Each inference engine is a separate Helm release with its own values-driven
chart:

- `helm/vllm-inference` — `make vllm-up` / `make vllm-down`. Installed by
  `make stack-up`, so it is the deployed definition of `vllm-qwen38-nvfp4`.
- `helm/sglang-inference` — `make sglang-up` / `make sglang-down`. Not part
  of `make stack-up`; an operator brings it up deliberately, after stopping
  vLLM.

Consequences of that shape:

- **GPU contention is now scheduled, not hoped for.** Both charts request
  and limit `nvidia.com/gpu: 1` with `strategy: Recreate` on a one-GPU node,
  so the second engine stays `Pending` instead of fighting for the card.
- **The engines stay out of the `airgap-stack` release.**
  `scripts/helm-render` keeps filtering `Deployment`/`Service`
  `vllm-qwen38-nvfp4` (`GPU_KEEP_OUT`), so a routine `helm upgrade` of the
  application stack never restarts a model server and never reloads a 27B
  checkpoint. `make gpu-objects-up` now applies only the two true
  prerequisites, `RuntimeClass/nvidia` and the model PV/PVC.
- **SGLang is reached over Service DNS**
  (`sglang-qwen38.airgap-ai-stack.svc.cluster.local:30000`) rather than
  `host.k3d.internal`, so `helm/airgap-stack/values.yaml`
  `inference.sglang.host` points into the cluster and Prometheus scrapes it
  as a normal target.
- **Its API key comes from the cluster.** The `sglang-api-key` Secret is
  created by the `airgap-stack` release when `SGLANG_API_KEY` is set in
  `.env`, and consumed by the SGLang chart. `make sglang-up` before
  `make helm-up` with an empty key leaves the pod unable to start.

`deploy/sglang-qwen38` and `deploy/llamacpp` remain as host-Docker paths for
bring-up and one-off benchmarking on a machine without a cluster. They are no
longer the wiring the gateway points at for SGLang.

## Consequences

- The `AIGatewayRoute` rule for a backend and the workload that serves it are
  now toggled in two different places: `inference.sglang.enabled` in
  `helm/airgap-stack/values.yaml` (currently `true`) advertises the model,
  `make sglang-up` actually runs it. Enabling the route without the release
  means `qwen38-nvfp4-sglang` appears in `/v1/models` and in Open WebUI's
  dropdown and fails on use. Keep the two in step, the same way ADR 0006
  already required for the host-run case.
- `k8s/overlays/remote-wsl-vllm-nvfp4/vllm.yaml` is now a second, unapplied
  copy of the vLLM Deployment that only feeds `make preflight` and the
  render. It contradicts [ADR 0002](0002-one-owner-per-object.md) in spirit;
  deleting it (and letting `helm-render` drop the `GPU_KEEP_OUT` entries with
  it) is open work. Until then the chart is authoritative and the overlay
  copy must not be treated as the deployed configuration.
- `make verify` does not lint or template either engine chart. Changes to
  them need `helm template` run by hand and a GPU to validate.
