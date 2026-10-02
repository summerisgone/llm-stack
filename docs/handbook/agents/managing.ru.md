# Управление агентами

[English](managing.md) | [Оглавление справочника](../README.ru.md)

Повседневная работа с флотом агентов: сборка и деплой, кто сейчас работает,
изменение каталога, ключи, offboarding пользователя и добавление нового
рантайма агента. Сначала прочитайте [агентов](README.ru.md) - там описаны
составные части.

**Содержание**

- [Сборка и деплой](#сборка-и-деплой)
- [Как посмотреть на флот](#как-посмотреть-на-флот)
- [Ключи](#ключи)
- [Изменение каталога](#изменение-каталога)
- [Ёмкость](#ёмкость)
- [Offboarding](#offboarding)
- [Диагностика](#диагностика)
- [Добавление рантайма](#добавление-рантайма)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Сборка и деплой

```sh
make agents-test                                      # unit-тесты agent-sync, брокера и адаптера
make agent-catalog AGENTS_PLATFORM=linux/amd64        # образ каталога, закрепляет AGENT_CATALOG_IMAGE
make agent-broker-image AGENTS_PLATFORM=linux/amd64   # llm-stack/agent-broker:local
make agent-adapter-images AGENTS_PLATFORM=linux/amd64 # образы pi и opencode, закрепляет PI_IMAGE / OPENCODE_IMAGE
make agents-k3d-load                                  # скопировать образы в узел k3d (по SSH)
make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents
make agents-smoke                                     # сквозная проверка, см. ниже
```

- `make agents-up` применяет оверлей (с подставленным публичным origin),
  пересоздаёт ConfigMap `agent-images` и перезапускает брокер. Работающие
  агенты продолжают работать; новые образы и каталог они получат при
  следующем старте.
- `make agents-down` масштабирует брокер в 0 и удаляет все поды агентов;
  профили остаются.
- `scripts/agents-smoke-test` создаёт двух временных пользователей и
  проверяет: аутентификацию брокера, онбординг, `/skills`, запуск по
  требованию, ответ модели (`AGENTS_SMOKE_INFERENCE=true`), pi и opencode,
  свойства песочницы (не root, нет токена, корень только на чтение, ядро
  gVisor), изоляцию между пользователями и рестарт брокера. Нужны
  `STACK_BASE_URL` и Python с TLS 1.3.

## Как посмотреть на флот

```sh
kubectl -n agents get pods,pvc -l app.kubernetes.io/managed-by=agent-broker -L agents.llm-stack/user-id
kubectl -n agents port-forward svc/agent-broker 18090:8080 &
curl -s localhost:18090/healthz                       # {"k":3,"running":..,"busy":..,"queued":..}
kubectl -n agents logs deploy/agent-broker | grep '"agent started"'   # с длительностью холодного старта
kubectl -n agents get pvc -o custom-columns=PVC:.metadata.name,USER:.metadata.annotations.agents\.llm-stack/username
```

Что делал агент, видно в Langfuse (трейсы под именем пользователя с
`pat_token_id` ключа агента) и в `patsvc_mcp_calls_total` для инструментов.

## Ключи

- Ключ выпускает сам пользователь на `/platform`. Больше ничего не нужно;
  поды перезапустятся с ним.
- Ключ истекает через `AGENT_PAT_TTL_DAYS` (7). Истёкший ключ проявляется
  как `HTTP 401` в ответе агента; лечится новым ключом на `/platform`.
- Запасной путь для оператора, для тестов или если дашборд сломан:
  `scripts/agent-inference-key` записывает PAT в Secret пользователя.
- Чтобы немедленно отрезать агентов пользователя, отзовите PAT агента на
  `/platform` или отключите учётную запись в Keycloak.

## Изменение каталога

Навыки, инструкции, настройки по умолчанию и MCP-серверы лежат в
`config/agents/base-profile/`:

| Изменение | Что править |
| --- | --- |
| Добавить навык | `skills/<name>/SKILL.md`; указать в `catalog.yaml`, если он обязательный или выключен по умолчанию |
| Инструкции агента | `SOUL.md` |
| Настройки Hermes и что пользователь может переопределить | `config.yaml`, `locked-keys.yaml` |
| Настройки pi или opencode | `runtimes/pi/*.json`, `runtimes/opencode/opencode.json` |
| MCP-серверы | `mcp-servers.yaml` ([MCP](../mcp/adding-a-server.ru.md)) |

Затем:

```sh
make agents-test
make agent-catalog AGENTS_PLATFORM=linux/amd64        # проверьте новый AGENT_CATALOG_IMAGE в versions.lock.env
make agents-k3d-load && make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents
```

Агенты получают новый каталог при следующем старте; о новых навыках
пользователи узнают в следующем ответе. Чтобы применить сразу:
`make agents-down && make agents-up` (обрывает текущие ходы).

## Ёмкость

| Настройка | По умолчанию | Где |
| --- | --- | --- |
| Запущенных агентов K | 3 | `AGENT_SLOTS` в `k8s/agents/broker.yaml`; держите `k8s/agents/quota.yaml` (pods = K + 1, суммы CPU и памяти) в согласии |
| Таймаут простоя | 15 мин | `IDLE_TIMEOUT` |
| Ожидание в очереди | 60 с | `SLOT_WAIT_TIMEOUT` |
| Ресурсы агента | CPU 100m / 1, память 384Mi / 1Gi | `AGENT_CPU_*`, `AGENT_MEMORY_*` |
| Размер профиля | 2 Gi | `PROFILE_SIZE` |

Модель агенты делят со всеми остальными: каждый шаг агента - обычный
PAT-запрос с rate limit-ом и полосой пользователя.

## Offboarding

```sh
scripts/agent-profile-delete <keycloak-sub|id>
```

Удаляет поды пользователя, Secret и PVC профилей всех рантаймов. Это
единственный путь удаления профиля; Role брокера не умеет удалять PVC.
Остальные данные пользователя: [удаление данных пользователя](../observability/data-and-retention.ru.md#удаление-данных-пользователя).

## Диагностика

| Симптом | Проверить | Исправить |
| --- | --- | --- |
| Агент отвечает `HTTP 401` | ключа нет или он истёк | новый ключ на `/platform` |
| Модель отвечает 404 в Open WebUI | образ рантайма не задан в `agent-images` | задать `PI_IMAGE` / `OPENCODE_IMAGE`, `make agents-up` |
| "All agent slots are busy" | `curl .../healthz`: `running` = K, простаивающих нет | подождать или поднять K вместе с квотой |
| Под висит в `ContainerCreating` на удалённом профиле | `kubectl -n agents describe pod`: нет обработчика RuntimeClass `gvisor` | настройка узла в [operations/agents.md](../../operations/agents.md#gvisor-on-k3dwsl2) |
| У агента нет MCP-инструментов | флаг в `.env`, ConfigMap `agent-images`, разложенная конфигурация в PVC | [диагностика MCP](../mcp/README.ru.md#диагностика) |
| Брокер отвечает 401 всем | `OIDC_ISSUER` не совпадает с `iss` токена | задать в оверлее issuer, видимый из браузера |

## Добавление рантайма

Новому рантайму агента (другому coding-агенту) нужно:

1. **Образ, говорящий на контракте брокера:** OpenAI chat completions с SSE
   на `:8642`, `GET /health`, bearer `API_SERVER_KEY`, всё состояние под
   `AGENT_HOME` на PVC, uid 10000, работа с корневой ФС только на чтение,
   никакого выхода наружу, кроме pat-service. Если у агента такого сервера
   нет, добавьте бэкенд в `agent-adapter/` (см. `pi.mjs`, `opencode.mjs`, `dsh.mjs`) и
   target в Dockerfile.
2. **Регистрацию в брокере:** запись `Runtime` в
   `agent-broker/cmd/agent-broker/main.go` (имя, id модели, заголовок,
   переменная образа `<NAME>_IMAGE`), плюс имя его компонента в селекторах
   NetworkPolicy (`k8s/agents/networkpolicy.yaml`) и в `pods.go`, если нужен
   особый env. Добавьте тест по образцу `TestAdapterRuntimePod`.
3. **Раскладку профиля:** `render_<name>` в `agent-catalog/agent_sync.py`
   (провайдер = pat-service с `AGENT_INFERENCE_KEY`, каталог навыков,
   инструкции, MCP-серверы из реестра) и настройки по умолчанию в
   `config/agents/base-profile/runtimes/<name>/`; тесты в `test_agent_sync.py`.
4. **Подключение к деплою:** закрепление образа в `versions.lock.env`, сборка
   в `make agent-adapter-images`, ключ в ConfigMap `agent-images`
   (`agents-up`), загрузка в `scripts/agents-k3d-load`.
5. **Open WebUI:** добавить id модели в `model_ids` подключения 2 в
   `openwebui-oidc-patch.yaml`, `make helm-up`.
6. **Smoke:** расширить `scripts/agents-smoke-test` и прогнать его с инференсом.
7. **Документацию:** таблица рантаймов в [агентах](README.ru.md) и этот
   список, на обоих языках.

## Где это лежит

- Runbook с настройкой узла под gVisor и перспективой Agent Substrate:
  [operations/agents.md](../../operations/agents.md)
- Скрипты: `scripts/agents-smoke-test`, `agent-profile-delete`,
  `agent-inference-key`, `agents-k3d-load`

## Связанные страницы

- Назад: [Агенты](README.ru.md). Дальше: [MCP](../mcp/README.ru.md)
