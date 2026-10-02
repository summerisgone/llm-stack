# Агенты

[English](README.md) | [Оглавление справочника](../README.ru.md)

У каждого пользователя могут быть персональные coding-агенты (Hermes, pi,
opencode, dsh), которые работают в собственном изолированном поде, хранят
постоянный профиль и пользуются моделью и MCP-инструментами с учётными
данными самого пользователя. Пользователь общается с ними как с моделями в
Open WebUI. `agent-broker` запускает их по требованию, держит запущенными
не больше K и останавливает простаивающих. Эксплуатация и развитие флота -
в [управлении агентами](managing.ru.md).

**Содержание**

- [Как это выглядит для пользователя](#как-это-выглядит-для-пользователя)
- [Архитектура](#архитектура)
- [Что работает в Kubernetes](#что-работает-в-kubernetes)
- [agent-broker](#agent-broker)
- [Рантаймы](#рантаймы)
- [Web UI dsh](#web-ui-dsh)
- [Профили и каталог](#профили-и-каталог)
- [Токены](#токены)
- [Изоляция и gVisor](#изоляция-и-gvisor)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Как это выглядит для пользователя

1. На `/platform` нажать **Issue agent key** (раз в неделю; ключ живёт 7 дней).
2. В Open WebUI выбрать `Hermes agent (personal)`, `Pi agent (personal)`,
   `OpenCode agent (personal)` или `DeepSeek agent (personal)`.
3. Первое сообщение создаёт профиль, и на него отвечает брокер текстом
   онбординга. Следующее сообщение запускает под агента; ответы стримятся
   как у любой модели.
4. `/skills` в чате показывает каталог навыков; `/skills on|off <name>` и
   `/skills reset` меняют выбор, который применяется при следующем старте
   агента.
5. У pi, opencode и dsh каждый чат Open WebUI - отдельная сессия агента,
   которая хранится в профиле и переживает перезапуски; Hermes ведёт сессии
   сам.
6. У агента dsh есть ещё собственный браузерный UI на хосте dsh
   ([Web UI dsh](#web-ui-dsh)): тот же вход, тот же профиль.

## Архитектура

```
Open WebUI (подключение 2, Keycloak-токен пользователя, X-OpenWebUI-Chat-Id)
  -> agent-broker (ns agents): проверить токен и роль, выбрать рантайм по имени модели,
     занять слот (запустить под / переиспользовать / вытеснить LRU простаивающего / поставить в очередь), проксировать чат
  -> под <runtime>-agent-<id> :8642 OpenAI chat-completions, bearer API_SERVER_KEY
       init catalog-sync: разложить профиль для этого рантайма
       агент: Hermes нативно или agent-adapter + pi / opencode / dsh
  -> pat-service /v1 (модель) и /mcp/<name>/ (инструменты), с PAT агента пользователя
```

`<id>` - первые 16 hex-символов `sha256(keycloak sub)`. Один под и один
профиль на пользователя и рантайм.

## Что работает в Kubernetes

| Объект | Имя | Кто создаёт |
| --- | --- | --- |
| Namespace | `agents` | `make agents-up` (`k8s/agents`) |
| Deployment, Service, ServiceAccount, Role, ConfigMap брокера | `agent-broker` | `make agents-up` |
| ConfigMap | `agent-images` (ссылки на образы, MCP-флаги) | `make agents-up` из `versions.lock.env` и `.env` |
| ResourceQuota | `agents` (pods 4 = K + брокер, суммы CPU и памяти) | `k8s/agents/quota.yaml` |
| NetworkPolicy | запрет по умолчанию; брокер - от Open WebUI, к Keycloak и API-серверу; агенты - только от брокера, только к pat-service | `k8s/agents/networkpolicy.yaml` |
| RuntimeClass | `gvisor` (удалённый профиль) | `k8s/overlays/remote-wsl-agents` |
| Pod | `<runtime>-agent-<id>` | брокер, по требованию |
| PVC | `<runtime>-profile-<id>` (2 Gi) | брокер, первое сообщение |
| Secret | `agent-cred-<id>`: `API_SERVER_KEY`, `INFERENCE_KEY` | брокер (ключ API), pat-service (ключ инференса) |
| Role в `agents` для pat-service | `pat-service-agent-key` | `k8s/agents/rbac.yaml` |

Всё, что создаёт брокер, помечено `app.kubernetes.io/managed-by:
agent-broker` и `agents.llm-stack/user-id: <id>`; аннотация PVC
`agents.llm-stack/username` называет пользователя.

```sh
kubectl -n agents get pods,pvc -l app.kubernetes.io/managed-by=agent-broker
```

## agent-broker

Небольшой Go-сервис (`agent-broker/cmd/agent-broker`) без базы данных:
состояние восстанавливается из кластера при старте.

- **API:** `GET /v1/models` (рантаймы, у которых задан образ) и
  `POST /v1/chat/completions` (модель `hermes-agent`, `pi-agent`,
  `opencode-agent` или `dsh-agent`), оба требуют Keycloak-токен с `ai-user` или `ai-admin`;
  `GET /healthz` отдаёт `{"k", "running", "busy", "queued"}`.
- **Слоты:** одновременно работают не больше `AGENT_SLOTS` (3) агентов,
  общих для всех пользователей и рантаймов. Новый старт занимает свободный
  слот, или вытесняет давнее всех использованного простаивающего агента
  (простаивает дольше `IDLE_TIMEOUT`, 15 мин), или ждёт в FIFO-очереди до
  `SLOT_WAIT_TIMEOUT` (60 с) с сообщением "все слоты заняты".
- **Устойчив к рестарту:** перезапущенный брокер подхватывает работающих
  агентов и считает их активными в течение одного idle timeout. Поды,
  удалённые в обход брокера, замечаются в течение 30 с.
- **Чаты:** брокер пересылает чат и `X-OpenWebUI-Chat-Id`; команды `/skills`
  и онбординг брокер обрабатывает сам.
- **Шов для бэкенда:** интерфейс `Backend` (`backend.go`) - место, куда
  подключился бы переход на Agent Substrate или kagent; сейчас это бэкенд
  `pods` ([ADR 0014](../../adr/0014-hermes-curated-catalog-and-worker-slots.md), раздел 10).

## Рантаймы

| Рантайм | Id модели | Образ | Внутри пода |
| --- | --- | --- | --- |
| Hermes | `hermes-agent` | `HERMES_IMAGE` (upstream) | `hermes gateway run`, OpenAI API-сервер на `:8642` |
| pi | `pi-agent` | `PI_IMAGE` (`agent-adapter`, target `pi`) | agent-adapter на `:8642`, pi SDK в процессе, расширение `pi-mcp-adapter` для MCP |
| opencode | `opencode-agent` | `OPENCODE_IMAGE` (`agent-adapter`, target `opencode`) | agent-adapter на `:8642`, `opencode serve` на loopback, управляется через его HTTP API |
| dsh (DeepSeek Harness) | `dsh-agent` | `DSH_IMAGE` (`agent-adapter`, target `dsh`) | agent-adapter на `:8642` управляет `dsh --profile acp` через stdio; `dsh web` на `:3080` для [web UI](#web-ui-dsh) |

У pi, opencode и dsh нет OpenAI-совместимого сервера, поэтому контракт брокера
обеспечивает `agent-adapter` (Node, `agent-adapter/`): OpenAI chat
completions на `:8642` со стримингом SSE (включая reasoning), `/health`,
bearer `API_SERVER_KEY`. Агенту он передаёт только последнее сообщение
пользователя; историю агент хранит в своей сессии (по id чата, на PVC).

Все рантаймы ходят к одной модели через pat-service, поэтому делят rate
limit пользователя и видны в Langfuse и QoS-дашбордах как этот
пользователь. Никакого другого выхода наружу у рантаймов нет: проверки
обновлений, телеметрия, каталоги моделей, установка плагинов и встроенные
веб-инструменты выключены.

## Web UI dsh

Под агента dsh также запускает `dsh web`, браузерный UI DeepSeek Harness,
только для этого пользователя
([ADR 0020](../../adr/0020-dsh-runtime-and-per-user-web-ui.md)). Он открывается
на собственном хосте `DSH_PUBLIC_ORIGIN`: dsh работает только от `/`, поэтому
под путём основного origin его не разместить.

```
браузер -> site proxy (TLS) -> edge Envoy: HTTPRoute dsh-web (по хосту)
  SecurityPolicy dsh-web: OIDC с Keycloak-клиентом dsh-web, access token передаётся дальше
  -> agent-broker :8081 web proxy: проверить токен и роль, при необходимости
     запустить под dsh-agent пользователя через слоты, проксировать HTTP + WebSocket
  -> под dsh-agent-<id> :3080 dsh web (тот же DSH_HOME и workspace, что у чатов)
```

- **Вход.** Только Keycloak SSO. Собственную cookie dsh (по launch token)
  брокер получает сам: берёт токен у agent-adapter и обменивает его.
  Пользователь токен не видит.
- **Изоляция.** Брокер выбирает под по subject токена, никогда по cookie;
  каждый пользователь видит только свои сессии и файлы.
- **Workspace.** `/opt/data/home/workspace` на PVC профиля: файлы переживают
  idle-эвикцию. Настройки модели и MCP зафиксированы каталогом.
- **Слоты.** Активностью считаются запросы страниц и API; одна открытая
  вкладка агента не удерживает. После эвикции следующий клик запускает его
  снова (холодный старт до нескольких минут).
- **Настройка.** `DSH_PUBLIC_ORIGIN` и `DSH_OIDC_CLIENT_SECRET` в `.env`,
  правило site proxy для этого хоста на edge listener (как у repowise), затем
  `make agents-up helm-up` (`helm-up` вызывает `provision-dsh-oidc`).

## Профили и каталог

Каждый PVC профиля - домашний каталог агента. При каждом старте
init-контейнер `catalog-sync` (образ `AGENT_CATALOG_IMAGE`,
`agent-catalog/agent_sync.py`) раскладывает его из каталога, вшитого в этот
образ (`config/agents/base-profile/`):

| Источник | Превращается в |
| --- | --- |
| `skills/` + `catalog.yaml` (обязательные, включённые или выключенные по умолчанию) + выбор пользователя через `/skills` | каталоги навыков `catalog/` и `catalog-enabled/` |
| `SOUL.md` + `SOUL.user.md` пользователя | инструкции агента (`SOUL.md`, `AGENTS.md` для pi, opencode и dsh) |
| `config.yaml` + `locked-keys.yaml` + `config.user.yaml` пользователя | `config.yaml` Hermes; заблокированные ключи всегда побеждают |
| `runtimes/pi/*`, `runtimes/opencode/opencode.json`, `runtimes/dsh/*` | `settings.json`, `models.json` pi; `opencode.json` opencode; `dsh/cordis.patch.yml` + `acp.patch.yml` + `web.patch.yml` dsh (провайдер = pat-service) |
| `mcp-servers.yaml` + флаги `*_ENABLED` | MCP-конфигурация каждого рантайма ([MCP](../mcp/README.ru.md#агенты)) |

Слой каталога принадлежит другому uid и доступен агенту только на чтение;
собственные файлы и сессии пользователя доступны на запись. Отчёт синхронизации
сохраняется в аннотации PVC `agents.llm-stack/sync-report`.

## Токены

- **Ключ инференса.** `/platform` -> "Issue agent key" вызывает pat-service
  `POST /api/agent-token`: тот отзывает предыдущий PAT агента пользователя,
  выпускает новый (`issued_by = agents`, TTL `AGENT_PAT_TTL_DAYS`, 7 дней),
  записывает его как `INFERENCE_KEY` в `agents/agent-cred-<id>` и удаляет
  работающие поды агентов пользователя, чтобы следующее сообщение запустило
  их с новым ключом. Каждый рантайм читает его как `AGENT_INFERENCE_KEY`
  (Hermes - ещё и как `HERMES_INFERENCE_KEY`) и отправляет в pat-service
  `/v1` и `/mcp/`. Брокер его не продлевает; когда ключ истекает,
  пользователь выпускает новый.
- **Брокер -> агент.** `API_SERVER_KEY` в том же Secret, генерирует брокер;
  агент принимает только запросы с ним.
- **Пользователь -> брокер.** Keycloak-токен пользователя из Open WebUI,
  проверяется по `OIDC_ISSUER` (issuer, видимый из браузера).

## Изоляция и gVisor

- Поды работают не от root (uid 10000), корневая ФС только на чтение, без
  токена ServiceAccount; на запись доступны только PVC профиля, `/work` и `/tmp`.
- Ключ подставляется из Secret, в спецификации пода его нет.
- NetworkPolicy: агенты принимают трафик только от брокера и ходят только в
  DNS и pat-service; ни агент-к-агенту, ни в Keycloak, ни в интернет.
- На удалённом профиле агенты работают под **gVisor** (`runtimeClassName:
  gvisor`, `AGENT_RUNTIME_CLASS` в оверлее `remote-wsl-agents`): каждый под
  получает ядро в пространстве пользователя (`uname` показывает `*-gvisor`),
  так что скомпрометированный агент атакует gVisor, а не ядро хоста.
  NetworkPolicy при этом действуют. Настройка узла (runsc и shim containerd
  в узле k3d) ручная, описана в [operations/agents.md](../../operations/agents.md#gvisor-on-k3dwsl2).
- "По требованию под gVisor" значит: под существует только пока агент
  пользователя используется (или простаивает в пределах таймаута), и каждый
  такой под - песочница gVisor.

## Где это лежит

- Брокер: `agent-broker/`; адаптер: `agent-adapter/`; каталог и
  синхронизация: `agent-catalog/`, `config/agents/base-profile/`
- Манифесты: `k8s/agents/`, оверлей `k8s/overlays/remote-wsl-agents/`
- Подключение Open WebUI: `k8s/overlays/remote-wsl-vllm-nvfp4/openwebui-oidc-patch.yaml`
- Выпуск токенов: `pat-service/cmd/pat-service/agents.go`
- Runbook: [operations/agents.md](../../operations/agents.md)

## Связанные страницы

- Назад: [Тюнинг](../configuration/tuning.ru.md). Дальше: [Управление агентами](managing.ru.md)
- [ADR 0009](../../adr/0009-cloud-hermes-fleet-per-user-profiles.md),
  [ADR 0014](../../adr/0014-hermes-curated-catalog-and-worker-slots.md),
  [ADR 0020](../../adr/0020-dsh-runtime-and-per-user-web-ui.md)
