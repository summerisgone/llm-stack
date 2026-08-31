# llama.cpp

`qwen-gguf.env` is a profile template for `deploy/llamacpp/run`: model path,
served name, port and GPU offload, read the same way
`config/sglang/qwen38.env` is. It ships with a placeholder `MODEL_GGUF` and
is not deployed by default -- `helm/airgap-stack/values.yaml` keeps
`inference.llamacpp.enabled: false` until a real, checksummed GGUF file is
in place and `MODEL_GGUF` points at it.

`N_GPU_LAYERS=0` (CPU-only) is the safe default: this host has one RTX 5090,
already claimed by vLLM or, when that is stopped instead, by SGLang (see
`config/sglang/model-profiles/README.md`). Only raise it while neither of
those is running.

Additional profiles (a different model, a tuned context size) get their own
`.env` file here, following the same four fields plus `N_GPU_LAYERS` and
`CTX_SIZE`.
