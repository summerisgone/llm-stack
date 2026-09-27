# Repowise

[English](repowise.md) | [Оглавление справочника](../README.ru.md)

Repowise индексирует фиксированный список репозиториев кода (wiki-страницы,
символы, полнотекстовый и векторный поиск, риск изменений) и отвечает на
вопросы о них. Люди пользуются его веб-интерфейсом на отдельном хосте;
модель и агенты - MCP-сервером `repowise` через pat-service. Эта страница -
эксплуатационный взгляд, который просил раздел 8
[ADR 0018](../../adr/0018-repowise-codebase-intelligence.md).

**Содержание**

- [Что работает](#что-работает)
- [Предусловия](#предусловия)
- [Деплой и обновление](#деплой-и-обновление)
- [Репозитории](#репозитории)
- [Доступ к веб-интерфейсу](#доступ-к-веб-интерфейсу)
- [Учётные данные](#учётные-данные)
- [Проверки](#проверки)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Что работает

Один Deployment `repowise` (релиз `helm/repowise`) с PVC на 20 Gi:

| Контейнер | Что делает |
| --- | --- |
| `workspace` (init) | клонирует репозитории и строит первичный индекс (`repowise reindex`), это может занять десятки минут |
| `app` | веб-интерфейс и API на порту 3000 |
| `mcp` | `repowise mcp` по streamable HTTP на порту 7338 (Service `repowise-mcp`) |
| `sync` | делает fetch каждые `syncIntervalSeconds` (900), чтобы индекс следовал за ветками |

Его вызовы LLM и эмбеддингов идут через pat-service `/v1` с сервисным PAT
(пользователь `svc-repowise`), поэтому ограничиваются, ставятся в очередь и
трассируются как у любого пользователя: `qwen-3.8-27b` для текстов и
`get_answer`, `bge-m3` для векторов. Thinking выключен
(`REPOWISE_REASONING=off`), иначе ответы приходили пустыми.

## Предусловия

- В `.env`: `REPOWISE_ENABLED=true`, `REPOWISE_PUBLIC_ORIGIN`,
  `STACK_BASE_URL`, `REPOWISE_API_KEY`, `REPOWISE_OIDC_CLIENT_SECRET`,
  `REPOWISE_PAT`.
- Эмбеддинги в режиме `accelerator: gpu` с одной готовой репликой
  (иначе `make repowise-up` откажется): индексирование на CPU-эмбеддингах
  душит узел. Бюджет GPU измеряли при ninfer в роли живого движка (пик
  около 2.2 GiB); vLLM при `gpuMemoryUtilization: 0.94` оставляет меньше,
  поэтому перед индексированием под vLLM уменьшите его
  ([KV-кэш](../inference/kv-cache.ru.md#как-стек-использует-кэш)).
- Правило на site-прокси, которое направляет хост repowise на тот же
  listener edge, что и основной origin.
- Клиент Keycloak `repowise`: `make provision-repowise-oidc`.
- Сервисный пользователь и его PAT: `scripts/kc-user-add`, затем
  `scripts/kc-pat-issue` ([scripts/README.md](../../../scripts/README.md)).

## Деплой и обновление

```sh
make provision-repowise-oidc        # один раз, после того как задан REPOWISE_OIDC_CLIENT_SECRET
make helm-up                        # pat-service /mcp/repowise/, запись tool server в Open WebUI
make repowise-up                    # Secret-ы из .env, релиз, рестарт Open WebUI
make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents   # профили агентов
```

`make repowise-down` удаляет релиз и его Secret-ы и перезапускает Open WebUI;
чтобы убрать инструмент отовсюду, выставьте флаг в `false` и выполните
`make helm-up` и `make agents-up`. Обновление - новый `REPOWISE_IMAGE` в
`versions.lock.env` (собирается `.github/workflows/repowise-image.yml`) плюс
`make repowise-up`.

## Репозитории

`repos` в `helm/repowise/values.yaml` (alias, URL, ветка). **Правило
допуска:** указывайте только репозитории, которые может читать каждый
`ai-user`. Каждый вошедший видит их все, в интерфейсе и через MCP; доступа
по пользователям у repowise нет. На этом этапе поддерживаются только
публичные HTTPS-репозитории (учётных данных в релизе нет). Добавление -
правка плюс `make repowise-up`; init-контейнер проиндексирует новый.

## Доступ к веб-интерфейсу

Интерфейс живёт на отдельном хосте (`REPOWISE_PUBLIC_ORIGIN`) за OIDC
Envoy Gateway с авторизацией по realm-ролям
(`helm/repowise/templates/route.yaml`):

| Роль | Может |
| --- | --- |
| `ai-user`, `ai-admin` | читать всё, пользоваться чатом и анализом blast radius |
| `repowise-admin` | всё: настройки, добавление и удаление репозиториев, sync, переиндексация |

Общий `REPOWISE_API_KEY` подставляется на edge; браузеры его не видят. Чаты
в интерфейсе общие для всех пользователей (пользователей у repowise нет).

## Учётные данные

| Secret | Откуда | Ротация |
| --- | --- | --- |
| `repowise` (`REPOWISE_API_KEY`, `OPENAI_API_KEY` = сервисный PAT) | `.env` через `make repowise-up` | новые значения в `.env`, `make repowise-up` |
| `repowise-credential` (`Bearer <api key>` для edge) | так же | так же |
| `repowise-oidc` (секрет клиента) | так же | также обновить клиент Keycloak: `make provision-repowise-oidc` |

Сервисный PAT истекает как любой PAT (до 365 дней); истёкший ломает
индексирование и `get_answer` с 401 в логах `app` и `mcp`.

## Проверки

```sh
kubectl -n airgap-ai-stack get pods -l app.kubernetes.io/name=repowise
kubectl -n airgap-ai-stack logs deploy/repowise -c sync --tail=20
```

Через дверь, с PAT: MCP-запрос `tools/list` на `pat-service /mcp/repowise/`
показывает 11 инструментов, и `patsvc_mcp_calls_total{server="repowise"}`
растёт с вызовами ([журнал проверок ADR 0018](../../adr/0018-repowise-codebase-intelligence.md#verification-log)).

## Где это лежит

- Чарт: `helm/repowise/` (`values.yaml`, `templates/workload.yaml`,
  `networkpolicy.yaml`, `route.yaml`); цель Makefile `repowise-up`
- Записи MCP-реестра: `config/agents/base-profile/mcp-servers.yaml`,
  `openwebuiToolServers` в `helm/airgap-stack/values.yaml`

## Связанные страницы

- Назад: [MCP](README.ru.md). Дальше: [Подключение MCP-сервера](adding-a-server.ru.md)
