# Model profiles

`../qwen38.env` is the "baseline" profile for the host-Docker bring-up path
(`deploy/sglang-qwen38/run`): the same `RadixArk-Qwen3.8-27B-NVFP4` checkpoint
vLLM serves. The engine that actually runs in the cluster is the
`helm/sglang-inference` release (`make sglang-up`), whose values hold the
`agents_nospec` variant -- a larger context and three concurrent sessions.
Keep the two in mind as separate profiles: editing this file changes nothing
about the deployed Deployment.

The in-cluster SGLang pods join the `qwen-3.8-27b` pool behind EPP by label
(ADR 0019); there is no SGLang-specific gateway setting.

**Mutual exclusion**: this host has one RTX 5090. Each engine pod requests
`nvidia.com/gpu: 1`, so set `VLLM_REPLICAS=0` and `SGLANG_REPLICAS=1` in
`.env` and run `make engines-up`; otherwise the SGLang pod stays `Pending`.
The host-Docker container in `deploy/sglang-qwen38` uses `--gpus all` outside
Kubernetes' view and gets no such protection — see
[ADR 0007](../../../docs/adr/0007-inference-engines-as-helm-releases.md).

Additional profiles (a new checkpoint, a new variant) get their own `.env`
file here, named after what they serve, following the same shape: model
path, served name, port, API key file, and the `sglang serve` flags as one
`SGLANG_ARGS` line.
