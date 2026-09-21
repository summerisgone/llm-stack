# 0016: Web search for Open WebUI -- self-hosted OpenSERP behind an engine-pinning sidecar, opt-in outside air-gapped installs

## Status

Proposed on 2026-09-21. Not deployed. Written from the repository and from
the upstream sources of Open WebUI `v0.11.3` (the pinned
`OPENWEBUI_VERSION`) and OpenSERP `v0.8.12` (`main` at `29c7b0f`,
2026-07-22); no live measurement was taken, so every assumption about the
host's egress, captcha rates and resource use is listed under
"Verification stages" and must be closed before this is accepted.

This is the first component whose purpose is to reach the public internet.
It does not change the air-gap guarantee of the stack: the web-search
release is opt-in and an air-gapped install simply does not enable it.

## Context

Users want chats in Open WebUI to be able to look things up on the web
(current versions, documentation, error messages, news) without the company
paying for a search API subscription (Tavily, Serper, SerpApi, Brave, Exa,
...) and without sending queries through a third-party SaaS account.

What Open WebUI `v0.11.3` already provides, read from its source:

- **A native OpenSERP provider.** `WEB_SEARCH_ENGINE=openserp`,
  `OPENSERP_BASE_URL` (default `http://localhost:7000`).
  `backend/open_webui/retrieval/web/openserp.py` calls
  `GET <base>/mega/search?text=<q>&limit=<n>` and maps `url/title/snippet`.
  It sends **no** `engines`, `mode`, `lang` or `region` parameter, and has
  no auth header.
- **A native SearXNG provider** (`SEARXNG_QUERY_URL`), which does forward
  language, safesearch and time range; and an in-process `duckduckgo`
  provider built on the `ddgs` library (no service at all). A generic
  `external` provider (`EXTERNAL_WEB_SEARCH_URL`, `POST {query,count}`) and a
  paid `yandex` (Yandex Search API) provider also exist.
- **Two usage modes.** The classic mode: the user toggles "Web search" in
  the chat, Open WebUI generates search queries with the task model, calls
  the provider, fetches the result pages itself with its web loader
  (`WEB_LOADER_ENGINE` empty = `safe_web`, plain HTTP from the Open WebUI
  pod) and injects them, by default through RAG embedding. The native
  (agentic) mode: with native function calling the model gets the builtin
  tools `search_web` and `fetch_url` (`backend/open_webui/tools/builtin.py`)
  and decides itself when to search and which links to open.
- **SSRF guard on page fetches.** `ENABLE_LOCAL_WEB_FETCH` defaults to
  `false` (private and cluster addresses are refused) and
  `WEB_FETCH_FILTER_LIST` adds metadata endpoints on top.
- **Configuration is env-only here.** The Deployment sets
  `ENABLE_PERSISTENT_CONFIG=false`, so anything configured in the admin UI
  is lost on restart; web search must be configured through env in the
  manifests like the rest of Open WebUI.
- **The default RAG embedding model is baked into the image**
  (`sentence-transformers/all-MiniLM-L6-v2`, downloaded at image build, CPU,
  in-process), so classic-mode RAG over web pages works offline, but that
  model is English-centric and weak for Russian queries.

What OpenSERP is, read from its source (`github.com/karust/openserp`, MIT):

- A Go service (fiber) that scrapes Google, Bing, Yandex, Baidu,
  DuckDuckGo and Ecosia by driving headless Chromium through go-rod
  (image base `chromedp/headless-shell`, non-root uid 1001, port 7000,
  `/health`). Browser rendering, sticky per-engine cookie "lanes" and
  resource blocking make it harder to block than plain-HTTP scrapers.
- `/mega/search` queries several engines in parallel, dedupes by normalized
  URL and returns merged results plus `clusters`. With no `engines`
  parameter it uses **all six**, and `app.mega_timeout` defaults to `90s`.
  There is no config key to disable an engine globally.
- On captcha an engine request returns `503 "captcha detected"`. A 2captcha
  solver exists but is off unless a paid key is configured.
- The API has **no authentication**. `cors.allow_origins` defaults to `*`.
  `proxies.allow_request_proxy_url` defaults to `true` and lets any caller
  pass `X-Proxy-URL`, and `/extract` fetches an arbitrary URL -- together an
  SSRF primitive for anything that can reach the pod.
- Per-engine rate limits (`rate_requests: 60`/min, `rate_burst: 3`),
  a 120s result cache, `max_processes: 6` Chromium processes, no Prometheus
  endpoint in the source (only `/health` and `/stats/cache`).
- Maturity: pre-1.0, one maintainer, ~1.4k stars. Scrapers break whenever
  an engine changes its markup.

What the repository constrains:

- One owner per object ([ADR 0002](0002-one-owner-per-object.md)); optional
  workloads with their own lifecycle are their own Helm releases, as for the
  engines ([ADR 0007](0007-inference-engines-as-helm-releases.md)) and
  embeddings ([ADR 0013](0013-embeddings-api-bge-m3.md)).
- Pinned images live in `versions.lock.env`; installs must work with no
  outbound network when the feature is off.
- There is **no `NetworkPolicy` anywhere in `k8s/` or `helm/`** today, so
  every pod already has unrestricted egress and any pod can reach any
  Service.
- One GPU, per-user fair share ([ADR 0008](0008-per-user-fair-share.md)).
  Open WebUI traffic lands in the lowest EPP band. Web search adds a
  query-generation call and much longer prompts to that traffic.

## Decision

### Provider: OpenSERP, SearXNG as the named fallback

OpenSERP is the provider, used through Open WebUI's native `openserp`
engine. Chosen because it needs no key or account, is supported upstream by
the pinned Open WebUI with no code or adapter, merges several engines into
one ranked list, and its browser-based scraping is the most likely of the
free options to keep Google and Yandex results working from a single
address.

SearXNG is the fallback, not a parallel deployment. It is chosen instead if
OpenSERP fails the V3 bake-off (below). The chart is shaped so the switch is
one value; nothing outside the chart and the Open WebUI env patch changes.

### Workload: its own release, `helm/web-search`

- New chart `helm/web-search`, release `web-search`, `make websearch-up` /
  `make websearch-down`, following ADR 0007/0013: its own `values.yaml`,
  outside the `airgap-stack` release, in namespace `airgap-ai-stack`.
- `values.yaml` carries `provider: openserp | searxng`. Air-gapped
  installs do not run `websearch-up` at all; `make stack-up` runs it only
  when `WEB_SEARCH_ENABLED=true` in `.env` (default `false`).
- Image pinned by digest in `versions.lock.env` as `OPENSERP_IMAGE`
  (and `SEARXNG_IMAGE` for the fallback). The sidecar image
  (`nginx-unprivileged`) pinned likewise.
- Pod: `replicas: 1`, ClusterIP Service `web-search:80`, non-root
  (uid 1001, as upstream), `readOnlyRootFilesystem` where the image allows,
  a memory-backed `emptyDir` on `/dev/shm` for Chromium, explicit CPU and
  memory `requests`/`limits` taken from V4, `app.max_processes` lowered from
  6 to what V4 shows the node can afford.

### OpenSERP configuration (ConfigMap-mounted `config.yaml`)

Everything not listed keeps the upstream default.

- `proxies.allow_request_proxy_url: false`, no `global` proxy, no pools.
- `extract.enabled: false` -- Open WebUI fetches pages itself; OpenSERP is
  used for SERPs only.
- `cors.enabled: false`.
- `captcha.solver_enabled: false`, no `2captcha` key (a paid service is
  what this ADR avoids).
- `app.mega_timeout: 20s` instead of 90s: a slow engine returns partial
  results rather than holding the chat for a minute and a half.
- `resilience.max_retries: 1`, circuit breaker enabled so an engine that is
  serving captchas is skipped instead of retried on every query.
- `cache.ttl_seconds` raised to 600: repeated queries within one
  conversation, and query-generation variants, hit the cache.

### Engine-pinning sidecar

Open WebUI sends only `text` and `limit`, so OpenSERP would query all six
engines, including Baidu, on every search. A small nginx sidecar in the same
pod is the Service's only port and:

- exposes exactly `GET /mega/search` and `GET /health`; everything else
  (`/extract`, `/<engine>/*`, `/stats/*`, image search) returns 404;
- appends the engine list and mode to the upstream query:
  `engines=google,bing,duckduckgo,yandex&mode=balanced` (the set is a
  value, `openserp.engines`, finalized by V3);
- drops any `X-Proxy-*`, `X-Use-Proxy`, `X-Use-Profile` request header;
- sets its own `proxy_read_timeout` slightly above `mega_timeout`.

OpenSERP itself listens on `127.0.0.1:7000` only, so the sidecar is not
bypassable from the network.

### Network boundary

- Ingress: a `NetworkPolicy` on the web-search pod admits only
  `app.kubernetes.io/name: openwebui` (and Prometheus, if a metrics
  exporter is added later). This is the first NetworkPolicy in the repo;
  V2 proves the k3d/k3s policy controller enforces it before anything
  relies on it.
- Egress: this pod is the only one whose job is to reach the internet. Its
  egress policy allows DNS and TCP 80/443 to non-private addresses and
  denies RFC 1918, link-local and the cluster CIDRs. Open WebUI's own page
  fetching keeps `ENABLE_LOCAL_WEB_FETCH=false` as its SSRF guard; a
  general egress policy for Open WebUI is out of scope here.
- No new public route. Nothing under the edge Gateway changes.

### Open WebUI configuration

A new patch in the remote overlay,
`k8s/overlays/remote-wsl-vllm-nvfp4/openwebui-web-search-patch.yaml`
(the overlay owns site patches; base stays provider-neutral and the
`local-mac` profile is untouched):

```
ENABLE_WEB_SEARCH=true
WEB_SEARCH_ENGINE=openserp
OPENSERP_BASE_URL=http://web-search.airgap-ai-stack.svc.cluster.local
WEB_SEARCH_RESULT_COUNT=5
WEB_SEARCH_CONCURRENT_REQUESTS=2
ENABLE_LOCAL_WEB_FETCH=false
ENABLE_WEB_LOADER_SSL_VERIFICATION=true
WEB_FETCH_MAX_CONTENT_LENGTH=<from V5>
BYPASS_WEB_SEARCH_EMBEDDING_AND_RETRIEVAL=<from V5>
```

- The patch and the release move together: the patch is only applied when
  `WEB_SEARCH_ENABLED=true`, so Open WebUI never points at a Service that
  does not exist. If that cannot be expressed cleanly in kustomize, the
  fallback is to apply the patch unconditionally with
  `ENABLE_WEB_SEARCH` driven from `.env` via `helm-env-values`, and record
  that here.
- Web search stays **off per chat by default**; the user turns it on with
  the "Web search" toggle. Native tool calling is allowed for the chat
  model; V6 establishes whether `search_web` is offered to the model only
  when the user enabled web search for that chat, because otherwise the
  model can send chat-derived queries to external engines without the user
  asking.
- `ENABLE_WEB_SEARCH_CONFIRMATION` stays `false`; the per-chat toggle is
  the consent point. Revisit if V6 shows native mode searches without it.
- Embedding vs. bypass: the baked-in MiniLM model is poor for Russian, and
  bypassing RAG puts whole pages into the prompt of a single shared GPU.
  V5 decides between (a) bypass with a hard content cap, and (b) keep RAG on
  MiniLM. Switching Open WebUI RAG to bge-m3 from ADR 0013 is a separate
  decision, out of scope here.

### Observability

- OpenSERP has no Prometheus endpoint. The sidecar's access log (status,
  upstream time) goes to the existing log pipeline; a blackbox probe of
  `/mega/search?text=<fixed>` every 5 minutes records availability and
  latency in Prometheus (`probe_success`, `probe_duration_seconds`).
- A Grafana panel on the cluster-monitor dashboard: probe success, p50/p95
  search latency, rate of `503` (captcha) from OpenSERP per engine from the
  structured logs.
- An alert when the probe fails for 30 minutes. A search that returns
  partial or empty merges (engines captcha'd, mega timeout) is a 200, not
  an error, so without the probe degradation is invisible to operators.

## Alternatives considered

| Option | Why not chosen (now) |
| --- | --- |
| Paid APIs (Tavily, Serper, SerpApi, Brave, Exa, Yandex Search API) | The goal is no subscription; also sends every query to one more third party under a company account |
| SearXNG | Mature, light, large community, forwards language/time range; but its engines scrape over plain HTTP and Google in particular blocks or captchas a single address quickly. Kept as the named fallback and the V3 comparison baseline |
| Open WebUI built-in `duckduckgo` (`ddgs`, in-process) | Zero infrastructure and a useful baseline, but runs inside the Open WebUI pod: no cache, no separate egress boundary, no engine control, failures invisible. Baseline in V3 only |
| YaCy | Own peer-to-peer index; result quality and freshness far below the big engines for a developer audience |
| Own adapter behind `EXTERNAL_WEB_SEARCH_URL` | Only worth it if we need logic Open WebUI and the sidecar cannot express (per-user quotas, query rewriting); code to own for no benefit today |
| OpenSERP without the sidecar | Every query hits all six engines including Baidu, 90s worst-case latency, unauthenticated `/extract` and `X-Proxy-URL` reachable by any pod |

## Consequences

- Chat users get web results with no API key, no account and no per-query
  cost. The cost is operational: a headless-browser scraper to keep alive.
- Search queries -- generated from chat content -- leave the company to
  Google, Bing, DuckDuckGo and Yandex from the site's egress address. Users
  must be told this in the client docs; confidential content should not be
  searched.
- Result availability depends on engines tolerating automated traffic from
  one address. Degradation is gradual (engines drop out of the merge) and
  must be watched, not assumed.
- Scraping search engines is contrary to their terms of service. This is a
  business decision for the company to take knowingly, not a technical one;
  the ADR does not settle it.
- Chats with web search are longer prompts on the single GPU, in Open
  WebUI's (lowest) EPP band; heavy web-search use slows the Open WebUI users
  themselves before it affects PAT traffic.
- The first `NetworkPolicy` enters the repo; from then on "no policy means
  open" is no longer uniformly true, and new workloads that must reach
  web-search must be added to its ingress rule.
- Air-gapped installs are unaffected: no release, no patch, no image.

## Risks

| Risk | Impact | Mitigation |
| --- | --- | --- |
| Google/Yandex captcha the site address under normal load | Few or no results; answers quietly degrade to "no results found" | Engine mix, cache, circuit breaker; V3 measures captcha rate over days; probe + alert; fallback to SearXNG or to a subset of engines |
| Upstream markup change breaks an engine parser | Engine returns nothing until an OpenSERP release | Multi-engine merge; pinned digest, upgrade after V3-style re-run; SearXNG fallback |
| Single-maintainer, pre-1.0 project goes stale | No fixes for broken parsers | MIT, small Go codebase, forkable; fallback is one value in `helm/web-search` |
| SSRF via `/extract` / `X-Proxy-URL` | Pod used to reach cluster services or pivot | Disabled in config, blocked in sidecar, OpenSERP bound to localhost, ingress policy, egress policy denies private ranges |
| NetworkPolicy not enforced on k3d | Both boundaries above are paper-only | V2 proves enforcement with a negative test before acceptance |
| Headless Chromium memory/CPU on the shared node | Keycloak, pat-service, Postgres slowed | Hard limits, `max_processes` from V4, `/dev/shm` sized explicitly |
| Chat content leaks through search queries | Confidential terms sent to external engines | Per-chat opt-in, client docs, V6 check of native-mode behavior; confirmation prompt as fallback |
| Native mode searches without the user's toggle | Queries leave without consent | V6; if confirmed, enable `ENABLE_WEB_SEARCH_CONFIRMATION` or disable `search_web` for the model |
| Page content floods the context | Long prefill on the shared GPU, context overflow, worse fair share for OWUI users | `WEB_SEARCH_RESULT_COUNT=5`, content cap, V5 measures prompt tokens per search |
| Russian queries poorly served by MiniLM RAG | Relevant passages dropped | V5 compares bypass vs RAG on Russian queries |
| Degraded search returns 200 with few or no results | Failure invisible to users and operators | Blackbox probe and alert; sidecar logs |
| Engines' terms of service | Legal/reputational exposure for the company | Explicit business sign-off before acceptance |

## Verification stages

Each stage has an exit condition. A failed gate stops the stages after it.

### V0 -- repository and render (no cluster)

- `helm template web-search helm/web-search -n airgap-ai-stack` clean for
  `provider: openserp` and `provider: searxng`.
- `make verify` clean with the Open WebUI patch applied and without it.
- Image digests present in `versions.lock.env`; install with
  `WEB_SEARCH_ENABLED=false` pulls none of them.

### V1 -- egress baseline

- From a debug pod: record the site's egress address and whether it is
  residential, business or datacenter; plain `curl` to each of the six
  engines' search pages -> status and whether a captcha page is served.

Exit: egress address type and per-engine first-contact result recorded
here.

### V2 -- boundaries

- `curl` to `web-search/mega/search?text=test` from the Open WebUI pod ->
  200 with results; from any other pod -> refused by NetworkPolicy.
- Through the sidecar: `/extract?url=...`, `/google/search`, `/stats/cache`
  -> 404; a request with `X-Proxy-URL` -> header not seen by OpenSERP
  (debug log).
- From the web-search pod: a private address (`keycloak` Service IP, the
  node IP) -> refused by egress policy; a public HTTPS host -> reachable.
- Open WebUI `fetch_url` / web loader on a URL resolving to a cluster IP ->
  refused (`ENABLE_LOCAL_WEB_FETCH=false`).

Exit: all assertions pass in `scripts/websearch-smoke-test`, run twice.

### V3 -- result quality and reliability bake-off

- Fixed set of at least 60 queries: one third Russian, one third English
  technical (error messages, library versions, docs), one third current
  events; each with a known good answer or source.
- Run through OpenSERP (engine set as configured and each engine alone),
  SearXNG with default engines, and Open WebUI's `duckduckgo` provider.
- Metrics: share of queries with a good source in the top 5, p50/p95
  latency, empty-result rate, captcha (`503`) rate per engine.
- Repeat the run daily for 7 days at realistic volume (V4 rate) to see
  whether engines start blocking.

Exit: provider and `openserp.engines` chosen from the numbers; if OpenSERP
is not better than SearXNG on quality and not worse on 7-day reliability,
`provider: searxng` is used and this ADR is amended before acceptance.

### V4 -- capacity

- Peak and steady CPU/memory of the pod at 1, 2 and 4 concurrent searches;
  `max_processes` and limits set from it.
- SSO login, pat-service `/healthz` and PAT validation latency while V4
  load runs.

Exit: values committed with the measurements they came from.

### V5 -- context cost and RAG mode

- For the V3 query set in classic mode: prompt tokens per search turn, TTFT
  of the chat model, with (a) bypass plus content cap, (b) MiniLM RAG.
- Answer quality on the Russian third of the set in both modes.

Exit: `BYPASS_WEB_SEARCH_EMBEDDING_AND_RETRIEVAL` and
`WEB_FETCH_MAX_CONTENT_LENGTH` set from the numbers.

### V6 -- native mode and consent

- With native function calling on the chat model: is `search_web` offered
  when the chat's web search toggle is off? Record observed behavior.
- Langfuse trace of a web-search chat shows the query-generation call and
  the final call with the injected sources.

Exit: consent behavior recorded; confirmation or tool restriction applied
if needed.

### V7 -- operations

- Blackbox probe up, Grafana panel populated, alert fires when the pod is
  scaled to zero.
- `make stack-up` with `WEB_SEARCH_ENABLED=true` from scratch brings search
  up with no manual step; with `false`, nothing web-search related exists.
- Client docs (Russian and English) in `docs/clients/README.md`: how to use
  the toggle, what leaves the company, what not to search for.
- Business sign-off on the terms-of-service question recorded here.

Exit: runbook in `docs/operations/web-search.md`, ADR moved to Accepted.

## Implementation order

1. `helm/web-search` with OpenSERP, sidecar, ConfigMap, Service; image
   pins; Makefile targets; `WEB_SEARCH_ENABLED` in `.env.example` (V0).
2. V1 from a debug pod -- before investing further if every engine already
   captchas the site address.
3. NetworkPolicies; `scripts/websearch-smoke-test`; V2.
4. Open WebUI overlay patch; V3 bake-off (SearXNG and `duckduckgo` run
   ad hoc for comparison, not committed as releases).
5. V4, V5; values committed.
6. V6; probe, dashboard, alert; client docs; V7; status to Accepted.

Out of scope: the `local-mac` profile, web search for PAT/API clients and
Hermes agents (they can call the same Service later under their own ADR),
switching Open WebUI RAG to bge-m3, image search, and a general egress
policy for the rest of the stack.
