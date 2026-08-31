# Inference tests

The local Mac profile (`k8s/overlays/local-mac`) is validated with
`make pat-smoke` against the LM Studio LAN model. The remote single-GPU
profile is exercised against the prepared k3d cluster: `make vllm-nvfp4-smoke`
validates the GPU vLLM Deployment directly, and `make llmd-nvfp4-smoke` runs
the EPP metrics check and the PAT end-to-end path against the `qwen-3.8-27b`
model served through the AI Gateway.

The remote profile is a connected-development / controlled-lab deploy only. A
production air-gapped bundle must preload model weights from an approved local
artifact.

The llm-d smoke deliberately excludes the existing Envoy global-rate-limit
assertion: it validates routing and EPP independently. Run `make pat-smoke`
when validating quota policy changes.

`make embeddings-smoke` (docs/adr/0013-embeddings-api-bge-m3.md V2) covers
the `bge-m3` embeddings API contract: 200/1024-dim/L2-normalized responses,
`/v1/models` listing, no-PAT and revoked-PAT 401s, and a max-size batch with
no 413. It is a no-op unless `EMBEDDINGS_SMOKE_ENABLED=true` -- the deployed
stack has `inference.embeddings.enabled: false` until an operator turns
ADR-0013 on -- so it is safe to leave in `make smoke-nogpu`. It does not yet
cover the 429 over-limit case, the over-length-item case, or confirming EPP's
`/metrics` counters are unchanged; add those once V2 is actually run against
a live cluster.