# Эксплуатация

[English](operations.md) | [Оглавление справочника](README.ru.md)

Указатель по повседневной работе: как попасть в кластер, выкатывать,
тестировать, управлять пользователями и узнавать сбои, которые у этого
стека уже случались. Подробности остаются в runbook-ах, на которые ссылается
эта страница.

**Содержание**

- [Доступ к кластеру](#доступ-к-кластеру)
- [Выкатка изменений](#выкатка-изменений)
- [Smoke-тесты](#smoke-тесты)
- [Пользователи и доступ](#пользователи-и-доступ)
- [Известные сбои](#известные-сбои)
- [Известные пробелы](#известные-пробелы)
- [Runbook-и](#runbook-и)
- [Связанные страницы](#связанные-страницы)

## Доступ к кластеру

Удалённый профиль - кластер k3d из двух нод на хосте Windows + WSL2:
`server-0` для control plane и приложений, `agent-0` для GPU
([ADR 0019](../adr/0019-inference-plane-gpu-worker-nodes.md)). С
рабочей машины `kubectl` ходит через SSH-туннель:

```sh
make k3s-tunnel            # на переднем плане; хост и порты из WSL_SSH_HOST / WSL_SSH_PORT в .env
kubectl --context wsl-llm-stack get nodes
```

Туннель обрывается каждые 20-40 минут: перезапускайте его, а долгие задачи
внутри подов запускайте в фоне (`nohup ... &` внутри контейнера), а не
через `kubectl exec` на переднем плане. Smoke-тестам против edge нужен
второй туннель к порту edge и `STACK_TUNNEL_PORT` либо `STACK_BASE_URL`,
указывающий на публичный origin. Детали хоста, туннель к edge и
восстановление после перезагрузки (контейнер сервера k3d часто приходится
запускать вручную) - в [operations/README.md](../operations/README.md).

## Выкатка изменений

| Изменение | Команда |
| --- | --- |
| Что угодно в манифестах или `helm/airgap-stack` | `make verify`, затем `make helm-up` |
| Живой движок | `INFERENCE_ENGINE` в `.env`, затем `make engine-up` ([движки](engines/README.ru.md#переключение-живого-движка)) |
| Флаги движка | `make vllm-up` / `sglang-up` / `ninfer-up` |
| EPP | `make llmd-up` |
| Дашборды, Grafana | `make monitoring-up` |
| Агенты | `make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents` ([управление агентами](agents/managing.ru.md)) |
| MCP-серверы | `make websearch-up`, `make repowise-up` |
| Новый код pat-service | push; CI собирает `PAT_SERVICE_IMAGE`; `kubectl -n airgap-ai-stack rollout restart deploy/pat-service` |
| Весь удалённый стек | `make stack-up` |

`make helm-diff` показывает, что изменит `helm-up`. `make down` масштабирует
прикладные Deployment-ы в ноль и сохраняет данные. Сообщения коммитов и PR -
по [AGENTS.md](../../AGENTS.md).

## Smoke-тесты

| Цель | Что доказывает | Что нужно |
| --- | --- | --- |
| `make verify` | рендер и проверки, ссылки в документации | ничего |
| `make smoke`, `make services-smoke` | SSO, редиректы, хранилища под операторами, отказы на edge | кластер |
| `make pat-smoke` | выпустить PAT, использовать, упереться в rate limit, отозвать | модель |
| `make llmd-nvfp4-smoke` | метрики EPP плюс сценарий PAT против `qwen-3.8-27b` | модель |
| `make smoke-nogpu` | всё выше без вызовов модели | кластер |
| `make embeddings-smoke` | `/v1/embeddings` | эмбеддинги |
| `make websearch-smoke` | границы веб-поиска | `WEBSEARCH_SMOKE_PAT` |
| `make agents-smoke` | флот агентов из конца в конец | Keycloak, модель для `AGENTS_SMOKE_INFERENCE=true` |
| `tests/telemetry/genai-smoke.sh` | трейсы доходят до Langfuse с содержимым и пользователем | модель |

Скрипты вычисляют адреса через `scripts/lib-endpoints.sh`: для удалённого
origin задайте `STACK_BASE_URL`. Скриптам, которые входят в систему
(`kc-pat-issue`), нужен Python с TLS 1.3, а не `/usr/bin/python3` из macOS.
Раскладка тестов: [tests/](../../tests/auth/README.md) (`auth`, `inference`,
`telemetry`, `qos`, `airgap`).

## Пользователи и доступ

Пользователи живут в Keycloak, realm `ai-stack`. Роли: `ai-user` (чат, PAT,
агенты), `ai-admin` (дополнительно администратор Open WebUI и Grafana
Admin), `repowise-admin` (настройки repowise). Вход по паролю в Open WebUI
выключен, всё идёт через Keycloak; смена роли действует со следующего входа.

```sh
scripts/kc-user-add <username> [role] [email] [first] [last]   # сгенерированный пароль, по умолчанию ai-user
scripts/kc-user-list [filter]
scripts/kc-user-delete <username>
scripts/kc-pat-issue <username> <password> [name] [days]       # PAT без браузера
```

Админ-консоль не опубликована (`/sso/admin` на edge отдаёт 404); для REST API
используйте `kubectl -n airgap-ai-stack port-forward svc/keycloak 8888:8080`,
именно так и делают скрипты. Offboarding пользователя затрагивает несколько
хранилищ: [удаление данных пользователя](observability/data-and-retention.ru.md#удаление-данных-пользователя).
Ротация учётных данных всех компонентов: [docs/security](../security/README.md).

## Известные сбои

| Симптом | Причина | Исправление |
| --- | --- | --- |
| Один пользователь Open WebUI получает 401 от модели | Open WebUI потерял сохранённую OAuth-сессию этого пользователя (`No OAuth session found`, в шлюзе `Jwt_is_missing`) | пользователь выходит и входит заново; `offline_access` (ставится `helm-up`) делает это редкостью |
| Все PAT разом падают с 401 | истёк закэшированный токен шлюза у pat-service (`Jwt_is_expired` в логе Envoy шлюза) | `kubectl -n airgap-ai-stack rollout restart deploy/pat-service`; проверить срок жизни токена клиента `pat-gateway` в Keycloak |
| Запросы висят, `llm_d_epp_ready_endpoints` 0, алерт stale-эндпоинтов | EPP не читает метрики движка: неверный селектор или метка движка, движок не Ready | `make engine-up` с правильным `INFERENCE_ENGINE`; если всё совпадает - перезапустить EPP (`make llmd-up` или удалить под) |
| Показатели KV и очереди в EPP застыли на 0 под нагрузкой | устаревшее состояние EPP после смены движка (растёт `llm_d_epp_datalayer_extract_errors_total`) | перезапустить EPP; если повторится - `router.epp.flags.v: 4`, чтобы поймать `extract failed` |
| Ложные 504 на длинных запросах | не хватает таймаута на edge или маршруте | таймауты - 10 минут на edge и маршруте ([инференс](inference/README.ru.md#таймауты-и-лимиты-на-пути)) |
| Запросы в JSON-режиме падают с 400 | живой ninfer отказывает в `response_format` | ожидаемо при `INFERENCE_ENGINE=ninfer` |
| Под движка в `Pending`, `Insufficient nvidia.com/gpu` | GPU занят другим движком | `make engine-up` сначала останавливает остальные |
| LLM-движок падает при старте после рестарта эмбеддингов | эмбеддинги на GPU заняли память первыми | порядок старта: движок, затем эмбеддинги (`engine-up` так и делает) |
| `kubectl` "connection refused" на порту туннеля | SSH-туннель оборвался | снова `make k3s-tunnel` |

## Известные пробелы

Вещи, которые заведомо не доделаны. Исправляйте их в стеке, когда
касаетесь этой области, и обновляйте этот список.

- Сроков хранения нет нигде; Langfuse растёт без ограничений ([данные и сроки хранения](observability/data-and-retention.ru.md)).
- Чарт llm-d ставится по digest, но образ EPP в нём - изменяемый тег
  `main` (`IfNotPresent`; digest в `versions.lock.env` записан, но не
  применяется), так что новый узел может скачать другую сборку.
- При `INFERENCE_ENGINE=vllm` и эмбеддингах на GPU `gpuMemoryUtilization:
  0.94` у vLLM оставляет меньше памяти GPU, чем измеренный пик эмбеддингов
  (ADR 0018 мерил его под ninfer); перед тяжёлой нагрузкой на эмбеддинги под
  vLLM его нужно уменьшить.
- Нет политик MCP по серверам или инструментам ([MCP](mcp/README.ru.md#политики-что-есть-и-чего-нет)).
- Нет метрик agent-broker; брокер не продлевает ключи агентов.
- `k8s/agents` применяется `make agents-up`, а не релизом `airgap-stack`.
- Вендоренный дашборд llm-d для SGLang запрашивает имена `sglang_*`, которых
  SGLang 0.5.19 не отдаёт.
- Часть старых документов (тексты ADR, заметки сессий в `CONTEXT.md`)
  описывает прежние состояния (`concurrency-detector`, имена `hermes-*`).
  ADR - это история и не редактируются; этот справочник описывает текущее
  состояние.

## Runbook-и

Runbook-и на английском.

| Область | Runbook |
| --- | --- |
| Доступ к хосту, туннели, администрирование Keycloak, трейсы Langfuse, восстановление после перезагрузки | [operations/README.md](../operations/README.md) |
| Движки и маршрутизация моделей | [operations/inference-backends.md](../operations/inference-backends.md) |
| Настройки и замеры vLLM | [operations/vllm-inference.md](../operations/vllm-inference.md) |
| Флот агентов, настройка узла под gVisor | [operations/agents.md](../operations/agents.md) |
| Веб-поиск | [operations/web-search.md](../operations/web-search.md) |
| Repowise | [mcp/repowise.ru.md](mcp/repowise.ru.md) |
| Установка новой площадки | [docs/install](../install/README.md) |
| Air-gap сборка | [docs/airgap](../airgap/README.md) |
| Безопасность и учётные данные | [docs/security](../security/README.md) |

## Связанные страницы

- Назад: [Подключение MCP-сервера](mcp/adding-a-server.ru.md). Дальше: [Как дописывать документацию](contributing.ru.md)
