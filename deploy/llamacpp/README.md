# llama.cpp server

Runs `llama.cpp`'s OpenAI-compatible server as a plain Docker container on
the WSL2 host, outside Kubernetes -- the same pattern
`deploy/sglang-qwen38` uses. The image is
`ghcr.io/ggml-org/llama.cpp:server-cuda`, pinned only by tag in
`versions.lock.env`: unlike the other pinned images in this repo it is a
rolling upstream tag with no published digest to pin against, so record the
digest yourself (`docker inspect --format '{{"{{"}}index .RepoDigests 0{{"}}"}}' <image>`)
after the first pull and update `versions.lock.env` for an air-gap bundle.

**Not deployed today.** `config/llamacpp/qwen-gguf.env` ships with a
placeholder `MODEL_GGUF`; `./run` refuses to start until it points at a real
file. Nothing else in the stack depends on this being present.

## Run and validate on the WSL2 host

```sh
# after filling in MODEL_GGUF in config/llamacpp/qwen-gguf.env
./run
./smoke
```

Both read defaults from `config/llamacpp/qwen-gguf.env`; override any of
them from the environment, e.g. GPU offload once the GPU is free:

```sh
N_GPU_LAYERS=99 ./run
```

## Wiring into the AI Gateway

Reached the same way SGLang is, at `host.k3d.internal:8090` (k3d's DNS alias
for this host's Docker bridge gateway) -- see
`deploy/sglang-qwen38/README.md` for why that address needs no site-specific
configuration. Once `./smoke` passes:

```sh
helm upgrade --install airgap-stack helm/airgap-stack \
  --namespace airgap-ai-stack --values helm/airgap-stack/values.yaml \
  --set inference.llamacpp.enabled=true
```

The model becomes selectable as `llamacpp-local`
(`inference.llamacpp.modelName`) through the same `/v1` route vLLM's
`qwen38-nvfp4` already uses. See
[docs/adr/0006-pluggable-inference-backends.md](../../docs/adr/0006-pluggable-inference-backends.md).
