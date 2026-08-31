# Repository Guidelines

## Project structure

This repository defines an air-gappable AI stack for Kubernetes. Read
[README.md](README.md) for what it is and [k8s/README.md](k8s/README.md) for
how the manifests are split.

The rule that matters most: **every object has exactly one owner.** Workloads
live in `k8s/base`; the `local-mac` overlay owns the development routing and
the LM Studio path; the `remote-wsl-vllm-nvfp4` overlay owns the GPU
prerequisites and the site patches, while `helm/airgap-stack/templates` owns
that profile's routing. Do not add a second copy of an object "for parity" —
that is exactly the drift
[ADR 0002](docs/adr/0002-one-owner-per-object.md) removed, and
`scripts/helm-render` now fails rather than let it happen again.

The model servers themselves are their own Helm releases:
`helm/vllm-inference` (`make vllm-up`, deployed by `make stack-up`) and
`helm/sglang-inference` (`make sglang-up`, opt-in) — see
[ADR 0007](docs/adr/0007-inference-engines-as-helm-releases.md). Edit their
`values.yaml`, not a manifest. `k8s/overlays/remote-wsl-vllm-nvfp4/vllm.yaml`
still holds an older copy of the vLLM Deployment; it is filtered out of every
release by `scripts/helm-render` (`GPU_KEEP_OUT`) and is **not** what runs.
The chart is authoritative.

Service configuration lives in `config/<service>/`, operational scripts in
`scripts/`, host and k3d bootstrap in `deploy/vllm-qwen38-nvfp4/`,
documentation in `docs/`, and acceptance tests in `tests/`. Pinned image and
Helm chart versions belong in `versions.lock.env`, which the Makefile
`include`s; never inline a version in a recipe. Model inventory and hashes
belong in `models/`.

## Inference queue: priority bands are not where they look

`config/llmd/router-nvfp4-values.yaml` declares **one** band under
`flowControl.priorityBands` (`priority: 0`), but the running EPP has **four**.
Bands `10`/`5`/`1` are provisioned dynamically by EPP from the
`InferenceObjective` objects listed in `router.inferenceObjectives`
(`warm`/`normal`/`demoted`), resolved in the `InferencePool`'s namespace —
`inferencePool.create: true` exists purely as that namespace anchor and must
**not** become the `AIGatewayRoute` backend (ADR 0002 routing is unchanged).
EPP logs `"Provisioning priority band from control plane"` for 1, 5 and 10 at
startup, and exposes `inference_extension_flow_control_*` for 0, 1, 5 and 10.
Do not conclude "one static band" from the values file alone — that reading is
wrong; see [ADR 0008](docs/adr/0008-per-user-fair-share.md) Stage 1B.

Consequences worth knowing before editing any of this:

- Higher priority is served first, so the static band `0` is the fallback for
  requests carrying no `x-llm-d-inference-objective` header, and it sits
  **below** `demoted` (1). Only `pat-service` sets that header; Open WebUI
  bypasses it and therefore lands in the lowest band.
- Explicit `fairnessPolicyRef`/`orderingPolicyRef` are set only on band `0`;
  the dynamic bands take EPP defaults.
- Negative priorities are deliberately left unused, reserved for a future
  sheddable background class.
- `concurrency-detector.maxConcurrency` must match the live engine's admission
  limit (SGLang `--max-running-requests`, vLLM `--max-num-seqs`) and moves
  together with `router.modelServers.*` and
  `core-metrics-extractor.defaultEngine` on every engine switch — see
  [docs/operations/inference-backends.md](docs/operations/inference-backends.md).

## Deployment host

The reference remote host, its SSH coordinates, the tunnels needed for
`kubectl` and the smoke tests, and the post-reboot recovery steps are in
[docs/operations](docs/operations/README.md). Standing the stack up somewhere
new is [docs/install](docs/install/README.md).

## Build, deployment, and validation

Copy `.env.example` to `.env` before deploying; never commit `.env`.

- `make verify` — everything checkable without a cluster: renders and
  client-validates all kustomizations, lints and templates `airgap-stack`,
  dry-runs the GPU prerequisite objects, and fails if the committed chart has
  drifted from the kustomize sources. Run this before every commit that
  touches manifests. It does **not** cover `helm/vllm-inference` or
  `helm/sglang-inference`; template those by hand when you change them
  (`helm template <release> helm/<chart> -n airgap-ai-stack`).
- `make preflight` — the kustomize half of `verify` on its own.
- `make stack-up` — the single deploy path for the remote GPU profile
  (prerequisites, GPU objects, the vLLM release, the `airgap-stack` release,
  llm-d, OIDC reconciliation). `make up` deploys the local-mac development
  profile.
- `make vllm-up` / `make vllm-down`, `make sglang-up` / `make sglang-down` —
  the inference engines on their own. One GPU: only one of them can run.
- `make down` scales application Deployments to zero without deleting PVCs.
- `make ps`, `make logs`, `make config` — workload state and rendered base.
- `make smoke`, `make services-smoke`, `make pat-smoke` — SSO, service and PAT
  boundaries. They work against both routing profiles: set `STACK_BASE_URL`
  for the remote origin, leave it unset for `*.localhost` development routes.
- `make llmd-nvfp4-smoke` — the remote profile end to end.
  `make smoke-nogpu` is the same coverage minus the calls that reach the
  model, for when the GPU is busy.

## Coding conventions

Two-space indentation for YAML; follow the surrounding JSON style. Shell
scripts are portable POSIX (`#!/usr/bin/env sh`) with `set -eu`, use `printf`
rather than shell-specific `echo`, and are named lowercase-hyphenated
(`scripts/services-smoke-test`). Smoke tests derive their endpoints from
`scripts/lib-endpoints.sh` and must not hardcode a hostname or a path layout.
Keep Kubernetes object names, labels and namespaces explicit and scoped to
their component. Do not commit credentials, private keys, model binaries or
generated kubeconfigs.

## Testing and changes

Run the smallest relevant validation for each changed boundary, and `make
verify` whenever manifests change. After changing a service boundary in an
inference overlay or chart, render it, client dry-run it, and run its smoke
test. Test scripts must fail nonzero on any unmet assertion. New tests go
under `tests/auth`, `tests/inference`, `tests/telemetry`, `tests/qos` or
`tests/airgap` with a note on how to run them.

A decision that would be expensive to reverse, or easy to undo by accident,
gets an ADR in `docs/adr/`. ADRs are not edited after acceptance — supersede
them with a new one.

## Commit and pull request guidelines

Concise imperative subjects, such as `Configure vLLM NVFP4 GPU profile`. Keep
commits focused. Pull requests state the affected services, the Kubernetes or
security impact, the validation commands run, and any cluster prerequisites.
Include logs or screenshots for user-visible changes, link relevant issues,
and never include secrets or model weights.
