ARG VLLM_BASE_IMAGE=vllm/vllm-openai@sha256:0a51ea5b4ae2dc5d81890e5173f54203d2a3ae0cfffe51b8fd2afd4391bfd967
FROM ${VLLM_BASE_IMAGE}

LABEL org.opencontainers.image.title="vLLM Qwen3.8 27B NVFP4"
LABEL org.opencontainers.image.description="Validated RTX 5090 profile for the RadixArk Qwen3.8 27B ModelOpt checkpoint"
LABEL org.opencontainers.image.source="https://github.com/vllm-project/vllm"

# The model is supplied as a read-only mount. Keep serving deterministic and
# prevent an accidental network fetch on the air-gapped deployment path.
ENV HF_HUB_OFFLINE=1 \
    TRANSFORMERS_OFFLINE=1 \
    VLLM_NO_USAGE_STATS=1

ENTRYPOINT ["vllm", "serve"]
CMD ["/model", "--served-model-name", "qwen38-nvfp4", "--language-model-only", "--dtype", "auto", "--kv-cache-dtype", "fp8", "--attention-backend", "TRITON_ATTN", "--max-model-len", "65536", "--max-num-seqs", "2", "--max-num-batched-tokens", "8192", "--gpu-memory-utilization", "0.90", "--reasoning-parser", "qwen3", "--enable-auto-tool-choice", "--tool-call-parser", "qwen3_coder"]
