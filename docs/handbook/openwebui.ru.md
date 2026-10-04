# Open WebUI

[English](openwebui.md) | [Оглавление справочника](README.ru.md)

Open WebUI - браузерное лицо стека. Пользователь входит через Keycloak,
выбирает модель или одного из своих агентов и общается; MCP-инструменты
включаются на уровне чата. Своей модели и значимого хранилища токенов у
Open WebUI нет: инференс идёт в приватный AI Gateway с собственным
Keycloak-токеном пользователя.

**Содержание**

- [Что получают пользователи](#что-получают-пользователи)
- [Как подключено](#как-подключено)
- [Подключения к моделям](#подключения-к-моделям)
- [Вход и роли](#вход-и-роли)
- [Чего ожидать](#чего-ожидать)
- [Эксплуатация](#эксплуатация)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Что получают пользователи

| В списке моделей | Что это | Через что идёт |
| --- | --- | --- |
| `qwen-3.8-27b` | сама модель, каким бы движком она ни обслуживалась | приватный AI Gateway -> EPP -> движок |
| `automations.*` | тот же каталог для Automations (запусков по расписанию) | pat-service `/v1` с общим PAT |
| `Hermes agent (personal)`, `Pi agent (personal)`, `OpenCode agent (personal)` | собственный агент пользователя, по одному на рантайм | agent-broker -> под агента пользователя |

Также каждому пользователю доступны (`USER_PERMISSIONS_*` в Deployment):
workspace-модели, knowledge, промпты, инструменты и навыки; собственные API
ключи Open WebUI; Automations. MCP-инструменты `web-search` и `repowise`
появляются в меню инструментов чата, если включены на площадке
([MCP](mcp/README.ru.md)); в каждом чате они выключены, пока пользователь их не
включит.

Не настроено: встроенный веб-поиск Open WebUI (`ENABLE_WEB_SEARCH=false`;
поиск - это MCP-инструмент), его RAG/embedding-настройки (значения Open WebUI
по умолчанию; bge-m3 стека к ним не подключён), генерация изображений и аудио.

## Как подключено

```
браузер --SSO--> Open WebUI --(Keycloak-токен пользователя)--> ai-gateway-private -> EPP -> движок
                     |--(общий PAT для automations)----------> pat-service /v1
                     |--(Keycloak-токен пользователя)--------> agent-broker (namespace agents)
                     \--(Keycloak-токен пользователя)--------> pat-service /mcp/<name>/   (инструменты)
```

- `auth_type: system_oauth` значит, что Open WebUI прикладывает к запросу
  access token вошедшего пользователя. Шлюз, брокер и pat-service проверяют
  его сами; никто не верит Open WebUI на слово.
- `ENABLE_FORWARD_USER_INFO_HEADERS=true` добавляет `X-User-Name` / `X-User-Id`,
  а для агентов - `X-OpenWebUI-Chat-Id`, по которому на каждый чат держится
  одна сессия агента.
- `TASK_MODEL_EXTERNAL=default`: заголовки чатов, теги, follow-up и поисковые запросы
  генерирует модель текущего движка, а не агент (иначе каждый был бы полным ходом агента).
- Трафик Open WebUI не проходит через pat-service, поэтому полосы приоритета
  у него нет: EPP кладёт его в запасную полосу `0`, самую нижнюю
  ([очереди](inference/queues-and-fair-share.ru.md)). В Langfuse у таких
  трейсов есть пользователь, но нет ключа сессии.

## Подключения к моделям

`OPENAI_API_BASE_URLS` и `OPENAI_API_CONFIGS` в
`k8s/overlays/remote-wsl-vllm-nvfp4/openwebui-oidc-patch.yaml` задают три
подключения по индексу:

| Индекс | Base URL | Аутентификация | Модели | Зачем |
| --- | --- | --- | --- | --- |
| `0` | `ai-gateway-private...:8080/v1` | `system_oauth` | `qwen-3.8-27b` (задана явно) | чат; каталога моделей, который Open WebUI мог бы обнаружить, у шлюза нет |
| `1` | `pat-service...:8080/v1` | `bearer` `OPENWEBUI_AUTOMATIONS_PAT` | обнаруживаются, префикс `automations` | Automations работают без браузерной сессии, им нужны статические учётные данные |
| `2` | `agent-broker.agents...:8080/v1` | `system_oauth` | `hermes-agent`, `pi-agent`, `opencode-agent`, `dsh-agent` (заданы явно) | персональные агенты; брокер отдаёт список моделей только по токену пользователя |

`ENABLE_PERSISTENT_CONFIG=false`: настройки, изменённые в админке, живут
только до перезапуска пода. Всё, что должно сохраняться, - переменные
окружения в манифестах. Новое имя модели или новый рантайм агента - правка
`model_ids` там же и `make helm-up`.

MCP tool server-ы берутся из ConfigMap `openwebui-tool-servers` (ключ
`TOOL_SERVER_CONNECTIONS`), который `helm/airgap-stack` рендерит из
`openwebuiToolServers` в своих values; читается только при старте. См.
[MCP](mcp/README.ru.md#open-webui).

## Вход и роли

- Вход только через Keycloak: `ENABLE_LOGIN_FORM=false`,
  `ENABLE_PASSWORD_AUTH=false`, `ENABLE_SIGNUP=false`; учётная запись
  создаётся при первом OIDC-входе (`ENABLE_OAUTH_SIGNUP=true`).
- Роли берутся из токена (`OAUTH_ROLES_CLAIM=realm_access.roles`,
  `ENABLE_OAUTH_ROLE_MANAGEMENT=true`): `ai-user` - обычный пользователь,
  `ai-admin` - администратор Open WebUI, любая другая учётная запись
  отклоняется (`OAUTH_ALLOWED_ROLES=ai-user,ai-admin`). Смена роли
  применяется при следующем входе.
- Init-контейнер (`sso-role-bootstrap`) создаёт пользователя-заглушку, чтобы
  первый настоящий SSO-пользователь не стал администратором автоматически.
- `OAUTH_SCOPES` содержит `offline_access`; на клиент Keycloak его выдаёт
  `scripts/provision-openwebui-offline-access` (запускается из
  `make helm-up`). Благодаря ему сохранённый refresh token живёт дольше
  SSO idle timeout Keycloak; без него простаивавшие пользователи молча
  теряют доступ к инференсу с 401.

Сами учётные записи управляются в Keycloak; см.
[эксплуатацию](operations.ru.md#пользователи-и-доступ).

## Чего ожидать

- **Имя модели не меняется.** `qwen-3.8-27b` обслуживает тот движок, который
  сейчас живой; смена движка для пользователя незаметна, кроме простоя во
  время самого переключения ([движки](engines/README.ru.md)).
- **На первое сообщение агенту** отвечает брокер, а не агент: текст
  онбординга, и создаётся профиль. Под агента стартует по требованию на
  следующем сообщении, после холодного старта (брокер пишет его в лог как
  `"agent started"`); когда все слоты заняты, пользователь видит сообщение
  об очереди ([агенты](agents/README.ru.md)).
- **Агенту нужен ключ агента.** Пока пользователь не нажал
  "Issue agent key" на `/platform`, ходы агента падают с 401 от модели.
- **Лимиты:** 60 запросов в минуту на пользователя на маршруте модели
  (выше - HTTP 429) и 30 вызовов MCP-инструментов в минуту на пользователя
  и сервер ([тюнинг](configuration/tuning.ru.md#rate-limit-ы)).
- **На `qwen-3.8-27b-ninfer`** движок отказывает в JSON-режиме
  `response_format`; зависящие от него функции (некоторые помощники для
  заголовков и инструментов) деградируют. `qwen-3.8-27b` это не касается.
- **Чаты хранятся** в SQLite Open WebUI на его PVC, и каждый промпт и ответ
  дополнительно трассируется в Langfuse
  ([данные и сроки хранения](observability/data-and-retention.ru.md)).

## Эксплуатация

| Симптом | Проверить | Исправить |
| --- | --- | --- |
| Один пользователь получает 401 от модели, остальные нет | лог Open WebUI `No OAuth session found for user`; лог шлюза `Jwt_is_missing` | пользователь выходит и входит заново (новый OIDC callback) |
| Tool server-а нет в меню чата | ConfigMap `openwebui-tool-servers` существует и содержит запись; флаг в `.env` | `make helm-up`, затем `kubectl -n airgap-ai-stack rollout restart deploy/openwebui` |
| Модель агента отвечает 404 | у брокера задан `PI_IMAGE` / `OPENCODE_IMAGE` | [агенты](agents/managing.ru.md) |
| Изменение в админке пропало после рестарта | `ENABLE_PERSISTENT_CONFIG=false` - так задумано | перенести настройку в манифест |

```sh
kubectl -n airgap-ai-stack logs deploy/openwebui --tail=100
kubectl -n airgap-ai-stack get configmap openwebui-tool-servers -o jsonpath='{.data.TOOL_SERVER_CONNECTIONS}'
```

## Где это лежит

- Deployment, env и PVC `openwebui-data` (SQLite `webui.db`):
  `k8s/base/applications.yaml`
- Патчи площадки (подключения, OIDC redirect, переключатель веб-поиска):
  `k8s/overlays/remote-wsl-vllm-nvfp4/openwebui-oidc-patch.yaml`,
  `openwebui-web-search-patch.yaml`
- Tool server-ы: `helm/airgap-stack/values.yaml` `openwebuiToolServers`,
  `helm/airgap-stack/templates/openwebui-tool-servers.yaml`
- Применяется `make helm-up`; образ закреплён в `versions.lock.env`
- Заметки по API для операторов: `.claude/skills/openwebui-api/SKILL.md`

## Связанные страницы

- Назад: [Обзор системы](overview.ru.md). Дальше: [PAT-сервис](pat-service.ru.md)
- [Агенты](agents/README.ru.md), [MCP](mcp/README.ru.md)
- [ADR 0003](../adr/0003-private-ai-gateway-and-pats.md): почему Open WebUI
  ходит в приватный шлюз с токеном пользователя
- [docs/security](../security/README.md)
