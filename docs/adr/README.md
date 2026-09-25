# Architecture decision records

Short records of decisions that are expensive to reverse or easy to undo by
accident. One file per decision, numbered, never edited after acceptance —
supersede instead.

| ADR | Decision |
| --- | --- |
| [0001](0001-path-routing-on-one-origin.md) | Public routing is by path on one canonical origin, not by Host |
| [0002](0002-one-owner-per-object.md) | Every Kubernetes object has exactly one owner: kustomize or the chart |
| [0003](0003-private-ai-gateway-and-pats.md) | The AI Gateway is cluster-only; programmatic access goes through PATs |
| [0004](0004-tls-terminates-at-the-site-proxy.md) | TLS terminates at the site proxy; everything behind it is plaintext |
| [0005](0005-legacy-nvidia-runtime-hook.md) | The GPU runtime uses the NVIDIA legacy prestart hook, not CDI |
| [0006](0006-pluggable-inference-backends.md) | Inference backends are added as AI Gateway rules keyed on model name, not route replacements |
| [0007](0007-inference-engines-as-helm-releases.md) | Each inference engine is its own in-cluster Helm release, outside the application release |
| [0008](0008-per-user-fair-share.md) | Per-user fair share of the 2-slot inference deployment; pat-service session identity feeds llm-d EPP fairness and priority bands (Proposed, draft) |
| [0009](0009-cloud-hermes-fleet-per-user-profiles.md) | Cloud Hermes fleet by per-user profiles; pool of stateless workers + state sidecar; Open WebUI is one front-end option, not the sole one |
| [0010](0010-sglang-hicache-kv-offload.md) | Keep SGLang KV on GPU; use the validated 180224-token GPU-only profile because hybrid Mamba HiCache is unstable |
| [0011](0011-pat-session-key-langfuse-tracing.md) | PAT's chain-hash session key maps to Langfuse `session.id`, grouping agent runs into one session |
| [0012](0012-graduated-band-ceiling-over-strict-priority.md) | Priority bands stop being absolute: graduated dispatch ceiling on a saturation signal with resolution |
| [0013](0013-embeddings-api-bge-m3.md) | Embeddings API: bge-m3 as its own release behind PATs, CPU by default, GPU as an opt-in memory loan from the LLM (Proposed) |
| [0014](0014-hermes-curated-catalog-and-worker-slots.md) | Hermes: curated base profile in the repo inherited by per-user PVC layers; `K` worker slots with idle eviction via a broker offered in Open WebUI as a model next to direct chat; broker-issued per-user PATs; no background agents; compute behind a backend interface -- plain pods by default, Agent Substrate as a gated experimental backend, AX not adopted (Proposed) |
| [0015](0015-third-party-engine-metrics-contract.md) | Metrics contract for third-party engines (llama.cpp, ninfer, ...): mandatory usage/scrape tiers vs. optional native-metric tier, and how to onboard a new provider (Proposed) |
| [0016](0016-self-hosted-web-search-openserp.md) | Web search for Open WebUI: self-hosted OpenSERP as its own opt-in release behind an engine-pinning, path-restricting sidecar; SearXNG as the fallback chosen by a bake-off; first NetworkPolicy in the repo (Superseded by 0017) |
| [0017](0017-web-search-mcp-openserp-kagent.md) | Web search as an MCP server: OpenSERP behind `web-search-mcp` (`web_search`, SSRF-guarded `fetch_url`), reached only through pat-service `/mcp/` with PAT or Keycloak token; Hermes via kagent `AgentHarness` and profile MCP entry, Open WebUI as MCP tool server; supersedes 0016 (Proposed) |
| [0018](0018-repowise-codebase-intelligence.md) | Repowise as one shared codebase-intelligence service: opt-in `helm/repowise` release; UI on its own host `repowise.<site>`, matched by `Host` on the existing edge listener (scoped exception to 0001), SSO by Envoy Gateway OIDC with the shared API key injected at the edge and a read/admin role split; MCP through a generalized pat-service `/mcp/<name>/`; LLM through pat-service under a service PAT; bge-m3 embeddings on the GPU with VRAM carved from the LLM engine as a precondition; everything indexed is visible to every `ai-user`; amends 0017 section 6 on tool-server ConfigMap ownership (Proposed) |
