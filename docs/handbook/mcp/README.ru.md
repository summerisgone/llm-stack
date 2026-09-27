# MCP-серверы

[English](README.md) | [Оглавление справочника](../README.ru.md)

MCP-серверы (Model Context Protocol) дают модели инструменты: веб-поиск и
загрузку страниц, вопросы по проиндексированным репозиториям кода. В этом
стеке каждый MCP-сервер стоит за **одной дверью** - pat-service
`/mcp/<name>/`, которая аутентифицирует вызывающего, добавляет идентичность,
ограничивает частоту вызовов инструментов и считает их. Open WebUI и все
рантаймы агентов доходят до серверов только через эту дверь. Подключение
нового - в [подключении сервера](adding-a-server.ru.md); эксплуатация
repowise - в [repowise](repowise.ru.md).

**Содержание**

- [Установленные серверы](#установленные-серверы)
- [Дверь: pat-service /mcp](#дверь-pat-service-mcp)
- [Open WebUI](#open-webui)
- [Агенты](#агенты)
- [Включение и выключение сервера](#включение-и-выключение-сервера)
- [Политики: что есть и чего нет](#политики-что-есть-и-чего-нет)
- [Диагностика](#диагностика)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Установленные серверы

| Имя | Инструменты | Апстрим | Флаг | Уходит ли за периметр |
| --- | --- | --- | --- | --- |
| `web-search` | `web_search`, `fetch_url` | `web-search-mcp` (Go, этот репозиторий) -> OpenSERP с nginx-sidecar, закрепляющим поисковики | `WEB_SEARCH_ENABLED` | да: запросы идут в публичные поисковики, загрузки - на публичные сайты |
| `repowise` | `get_overview`, `get_context`, `search_codebase`, `get_symbol`, `get_answer`, `get_risk`, `get_change_risk`, `get_why`, `get_dead_code`, `get_health`, `list_repos` | `repowise mcp` (upstream v0.53.0) над проиндексированными репозиториями | `REPOWISE_ENABLED` | скачивает перечисленные публичные репозитории |

Оба флага в `.env.example` равны `false` и должны оставаться выключенными в
air-gap установке. Решения: [ADR 0017](../../adr/0017-web-search-mcp-openserp-kagent.md)
(веб-поиск через MCP; [ADR 0016](../../adr/0016-self-hosted-web-search-openserp.md)
- нагрузка OpenSERP), [ADR 0018](../../adr/0018-repowise-codebase-intelligence.md)
(repowise).

## Дверь: pat-service /mcp

```
агент (PAT)                  --\
                                > pat-service /mcp/<name>/  -- X-User-Id, X-User-Name, X-Pat-Token-Id -->  MCP-сервер <name>
Open WebUI (JWT пользователя) --/     аутентификация, rate limit, метрика                              (NetworkPolicy: подключаться может только pat-service)
```

| Шаг | Поведение | Код |
| --- | --- | --- |
| Маршрут | `GET|POST|DELETE /mcp/<name>/` для имён из таблицы серверов, чей флаг `"true"`; любое другое имя - 404 | `pat-service/cmd/pat-service/main.go` (таблица), `mcp.go` |
| Аутентификация | `Authorization: Bearer` с PAT (`sk-...`, не отозван и не истёк) или JWT Keycloak с realm-ролью `ai-user` или `ai-admin`; иначе 401 или 403 | `mcpAuthenticate` |
| Идентичность | срезает клиентские заголовки идентичности и cookie, ставит `X-User-Id` (sub), `X-User-Name`, `X-Pat-Token-Id` (для PAT) | `forwardMCP` |
| Rate limit | только `tools/call`, на пользователя и сервер, фиксированное минутное окно в Valkey; `MCP_CALLS_PER_MINUTE` (30); 429 с `Retry-After: 60`; при ошибке Valkey пропускает | `mcpLimited` |
| Метрика | `patsvc_mcp_calls_total{user,server,tool,status}`; tool - имя для известных инструментов, иначе `other` | `mcpToolName` |
| Транспорт | reverse proxy с прямой передачей стрима (SSE); недоступный апстрим - 502 | `forwardMCP` |

URL апстрима по умолчанию - его внутрикластерный Service, переопределяется
через `WEB_SEARCH_MCP_URL` / `REPOWISE_MCP_URL`. Публичного маршрута у
`/mcp/` нет: он доступен только внутри кластера.

Сами MCP-серверы аутентификации не имеют (у транспорта repowise её нет и в
upstream). Заголовкам идентичности они доверяют только потому, что их
NetworkPolicy пропускает лишь поды pat-service, так что **NetworkPolicy -
часть модели безопасности**, а не оптимизация.

## Open WebUI

- `openwebuiToolServers` в `helm/airgap-stack/values.yaml` перечисляет
  серверы с их флагами. `templates/openwebui-tool-servers.yaml` рендерит
  включённые в ConfigMap `openwebui-tool-servers`, ключ
  `TOOL_SERVER_CONNECTIONS`, как MCP-подключения к
  `http://pat-service...:8080/mcp/<name>/` с `auth_type: system_oauth`
  (собственный Keycloak-токен пользователя чата; общего ключа нет) и правом
  чтения для всех пользователей.
- Open WebUI читает его только при старте: после изменения - `make helm-up`
  и перезапуск Open WebUI (`websearch-up`, `repowise-up` и их `-down` делают
  это сами).
- В чате инструменты выключены, пока пользователь не включит их в меню
  инструментов чата; это и есть точка согласия на отправку данных в интернет.
- Встроенный веб-поиск Open WebUI выключен (`ENABLE_WEB_SEARCH=false`), так
  что поиск всегда идёт через эту дверь.

## Агенты

Все рантаймы управляются одним реестром,
`config/agents/base-profile/mcp-servers.yaml`:

```yaml
pat_service: http://pat-service.airgap-ai-stack.svc.cluster.local:8080
servers:
  web-search: {flag: WEB_SEARCH_ENABLED, timeout: 60}
  repowise:   {flag: REPOWISE_ENABLED, timeout: 180}    # get_answer запускает LLM
```

При каждом старте агента `catalog-sync` оставляет записи, чей флаг `"true"`
(флаги приходят из `.env` через ConfigMap `agent-images` и брокер), и
раскладывает их под каждый рантайм, с аутентификацией PAT-ом агента
пользователя:

| Рантайм | Файл в профиле | Вид |
| --- | --- | --- |
| Hermes | `config.yaml` `mcp_servers.<name>` | `url`, `headers.Authorization: Bearer ${HERMES_INFERENCE_KEY}`, `timeout` (с); каждое имя из реестра - заблокированный ключ, так что пользователь не может добавить свою копию или оставить выключенную |
| pi | `pi/mcp.json` для `pi-mcp-adapter` | `url`, `headers`, `lifecycle: keep-alive`, `directTools: true` (инструменты видны как обычные инструменты pi), `requestTimeoutMs`; `allowInstall: false`, `hostConfigDiscovery: off` |
| opencode | `opencode/opencode.json` `mcp.<name>` | `type: remote`, `url`, `headers` с `{env:AGENT_INFERENCE_KEY}`, `timeout` (мс), `oauth: false` |

Встроенные веб-инструменты агентов выключены (toolset `web` у Hermes,
`webfetch`/`websearch` у opencode), так что доступ агента в веб - либо этот
сервер, либо никакой. Поды агентов могут ходить только в pat-service, так
что обойти дверь они не могут.

## Включение и выключение сервера

Флаг в `.env` - единственный переключатель. Он доходит до четырёх мест,
каждое применяется своей целью:

| Место | Чем применяется |
| --- | --- |
| релиз сервера (`helm/web-search`, `helm/repowise`) | `make websearch-up` / `websearch-down`, `make repowise-up` (`stack-up` запускает `websearch-up`, если флаг включён) |
| pat-service `/mcp/<name>/` | `make helm-up` (затем рестарт pat-service, если под не перекатился) |
| tool server в Open WebUI | `make helm-up` плюс рестарт Open WebUI |
| профили агентов | `make agents-up`; агенты подхватят при следующем старте |

Runbook-и: [operations/web-search.md](../../operations/web-search.md),
[repowise](repowise.ru.md).

## Политики: что есть и чего нет

| Контроль | Есть ли | Где |
| --- | --- | --- |
| Сервер включён или выключен для всей площадки | да | флаги `*_ENABLED` |
| Вызывать могут только `ai-user` / `ai-admin` | да | `mcpAuthenticate` (для JWT); владельцы PAT - пользователи, у которых была роль в момент выпуска PAT |
| Rate limit вызовов инструментов на пользователя | да, одно значение для всех серверов | `MCP_CALLS_PER_MINUTE` |
| Отзыв PAT отрезает сразу и инференс, и инструменты | да | поиск PAT |
| Пользователь не может добавить или сохранить MCP-серверы в Hermes | да | заблокированные ключи |
| Ограничения исходящего трафика сервера | да | NetworkPolicy в `helm/web-search`, `helm/repowise`; `fetch_url` отказывает для приватных, кластерных и metadata-адресов |
| Что видит пользователь repowise | все перечисленные репозитории для всех пользователей | `repos` в `helm/repowise/values.yaml` (правило допуска: только репозитории, которые может читать каждый `ai-user`) |
| Allowlist серверов или инструментов по пользователю или роли | **нет** | каждый аутентифицированный пользователь может вызвать любой включённый инструмент |
| Разные rate limit-ы по серверам или ролям | **нет** | один `MCP_CALLS_PER_MINUTE` |
| Отдельное включение сервера для Open WebUI и для агентов | **нет** | один флаг управляет обоими |

Новая политика из недостающих относится к обработчику `/mcp` в pat-service
(единственному месту, через которое проходят все вызовы) и обычно
заслуживает ADR.

## Диагностика

| Симптом | Проверить |
| --- | --- |
| 404 на `/mcp/<name>/` | флаг не `"true"` в env pat-service: `make helm-up`, рестарт pat-service |
| 401 / 403 | PAT отозван или истёк; JWT без `ai-user`/`ai-admin` |
| 429 | достигнут `MCP_CALLS_PER_MINUTE`; см. `patsvc_mcp_calls_total{status="429"}` |
| 502 `<name> unavailable` | под сервера или его Service |
| У агента нет инструментов | разложенная конфигурация в профиле, например `kubectl -n agents exec hermes-agent-<id> -c hermes -- grep -A4 mcp_servers /opt/data/home/config.yaml`; флаг в `agent-images` |
| В Open WebUI нет инструмента | ConfigMap `openwebui-tool-servers` и рестарт Open WebUI |

## Где это лежит

- Прокси: `pat-service/cmd/pat-service/mcp.go`, таблица серверов в `main.go`
- Серверы: `web-search-mcp/`, `helm/web-search/`, `helm/repowise/`
- Open WebUI: `openwebuiToolServers` в `helm/airgap-stack/values.yaml`
- Агенты: `config/agents/base-profile/mcp-servers.yaml`, `agent-catalog/agent_sync.py`
- Smoke: `make websearch-smoke`; unit-тесты: `make web-search-mcp-test`,
  `cd pat-service && go test ./...`, `make agents-test`

## Связанные страницы

- Назад: [Управление агентами](../agents/managing.ru.md). Дальше: [Repowise](repowise.ru.md)
- [PAT-сервис](../pat-service.ru.md), [Open WebUI](../openwebui.ru.md), [Агенты](../agents/README.ru.md)
