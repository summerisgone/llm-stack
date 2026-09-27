# Data and retention

[Русский](data-and-retention.ru.md) | [Handbook index](../README.md)

What the stack stores, where, whether it is personal data, and how long it is
kept. The short answer on retention: **nothing in the repository sets a
retention period.** Every store runs on its upstream default or keeps data
until someone deletes it. This page is the inventory to start from when that
has to change.

**Contents**

- [Inventory](#inventory)
- [Retention today](#retention-today)
- [Deleting a user's data](#deleting-a-users-data)
- [Setting retention](#setting-retention)
- [Where it lives](#where-it-lives)
- [Related](#related)

## Inventory

| Store | Where | Content | Personal data |
| --- | --- | --- | --- |
| Langfuse traces | ClickHouse `langfuse-clickhouse` (20 Gi), Postgres `langfuse-db` (10 Gi), MinIO bucket `langfuse` (10 Gi) | full prompts and answers, model, tokens, latency, user name, Keycloak `sub`, PAT id, session id | **yes, including conversation content** |
| Open WebUI | SQLite `webui.db` on PVC `openwebui-data` (5 Gi) | users, chats, files, workspace items, encrypted OAuth tokens | yes, conversation content |
| Keycloak | Postgres `keycloak-db` (5 Gi) | accounts, roles, sessions, TOTP secrets where configured | yes |
| pat-service tokens | Postgres `pat-db`, table `personal_access_tokens` | HMAC hash, 15-char prefix, owner `sub` and name, dates, `issued_by` | yes (identifiers) |
| pat-service usage | same, table `qos_events` | one row per chat or embeddings response: owner, session id, model, band, tokens, cost, token id | yes (identifiers, no content) |
| Valkey `envoy-ratelimit-valkey` | memory, no persistence | rate-limit counters, session chains, spend windows | identifiers, short-lived |
| Prometheus (application) | PVC `prometheus-data` (10 Gi) | metrics; `user` and `fairness_id` labels are Keycloak `sub` values | identifiers |
| Loki, Tempo, add-ons Prometheus | `monitoring` namespace, eg-addons chart | container logs, Envoy traces and metrics | logs can contain identifiers |
| Agent profiles | PVC `<runtime>-profile-<id>` in `agents` | agent sessions and history, personal skills, settings | **yes, conversation content** |
| Agent keys | Secret `agent-cred-<id>` in `agents` | the user's current agent PAT | credential |
| ninfer request log | `emptyDir` in the ninfer pod | per-request timings and token counts | no content; lost on restart |
| repowise | its own 20 Gi PVC (`helm/repowise`) | indexed public repositories | no |

## Retention today

| Store | Setting in the repo | Effective behaviour |
| --- | --- | --- |
| Langfuse (ClickHouse, Postgres, MinIO) | none | kept until deleted in Langfuse; no ClickHouse TTL, no MinIO lifecycle rule |
| Open WebUI | none | kept until the user or an admin deletes chats |
| pat-service `personal_access_tokens` | none | rows kept forever; revoked and expired tokens stay with their dates |
| pat-service `qos_events` | none | kept forever; no purge job |
| Valkey QoS keys | TTLs in code | sessions and chains 30 min (`QOS_SESSION_TTL_SECONDS`), spend 10 min (`QOS_SPEND_WINDOW_SECONDS`), MCP windows 2 min, active-user sets pruned every 30 s |
| Prometheus (application) | none (no `--storage.tsdb.retention.*` flag) | Prometheus default: 15 days, bounded by the 10 Gi PVC |
| Loki, Tempo, add-ons Prometheus | none (chart defaults) | see the eg-addons chart version in `versions.lock.env` |
| Agent profiles | none | kept until `scripts/agent-profile-delete` |
| Keycloak | realm defaults | sessions expire by realm settings; accounts stay |

Two consequences worth knowing before a load test or an audit:

- **Prometheus history is short.** Anything older than about two weeks is
  gone; ADRs quote numbers as fixed text for that reason. Export what you
  need to keep.
- **Langfuse grows without bound** and holds the most sensitive data in the
  stack. Watch the ClickHouse and MinIO PVC usage.

## Deleting a user's data

There is no single command. For a departing user:

1. Keycloak: disable or delete the account (their PATs then fail).
2. `/platform` or SQL: revoke remaining PATs (`revoked_at`); rows stay.
3. Agents: `scripts/agent-profile-delete <keycloak-sub|id>` deletes pods,
   Secret and all profile PVCs ([agents](../agents/managing.md#offboarding)).
4. Open WebUI: delete the user in the admin panel (removes their chats).
5. Langfuse: traces are keyed by user name and `sub`; delete them in
   Langfuse (UI or API) by user.
6. `qos_events`: delete rows by `owner_subject` if required.

## Setting retention

Not implemented; where it would go:

| Store | Mechanism |
| --- | --- |
| Prometheus | `--storage.tsdb.retention.time` / `.size` args in `k8s/base/observability.yaml` |
| Langfuse | Langfuse's own data-retention setting, if the deployed version offers it, or ClickHouse TTLs; `config/langfuse/` is the reserved place for its configuration |
| MinIO | a lifecycle (ILM) rule on bucket `langfuse` |
| `qos_events` | a scheduled `DELETE ... WHERE created_at < now() - interval ...` (CronJob against `pat-db`) |
| Loki | the Loki subchart's retention settings under the `loki` key of `config/gateway-addons/values.yaml` (take the exact path from the eg-addons chart's own values) |

A retention decision is expensive to reverse (data is gone), so record it in
an ADR ([contributing](../contributing.md)).

## Where it lives

- Stores: `k8s/base/databases.yaml`, `k8s/base/object-storage.yaml`,
  `k8s/base/applications.yaml`, `k8s/base/observability.yaml`
- Valkey TTLs: `pat-service/internal/qos/redis.go`,
  `pat-service/cmd/pat-service/main.go`
- Credentials for these stores: [docs/security](../../security/README.md#credential-inventory)

## Related

- Previous: [Langfuse](langfuse.md). Next: [Configuration](../configuration/README.md)
