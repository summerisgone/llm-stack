# Обзор системы

[English](overview.md) | [Оглавление справочника](README.ru.md)

Из чего состоит стек, что в каком namespace, и три способа, которыми запрос
доходит до модели. Каждая следующая страница справочника увеличивает один
из блоков схем ниже.

**Содержание**

- [Одним предложением](#одним-предложением)
- [Компоненты](#компоненты)
- [Namespace-ы](#namespace-ы)
- [Три пути запроса](#три-пути-запроса)
- [Идентичность в запросе](#идентичность-в-запросе)
- [Кто чем владеет в репозитории](#кто-чем-владеет-в-репозитории)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Одним предложением

Kubernetes-стек, который ставит одну границу идентичности (Keycloak) перед
локальной LLM: люди общаются в Open WebUI, программы вызывают
OpenAI-совместимый `/v1` с персональными токенами (PAT), персональные агенты
работают в изолированных подах, а всё это справедливо ставится в очередь
llm-d EPP перед одним движком на GPU, трассируется в Langfuse и измеряется в
Prometheus/Grafana.

## Компоненты

| Компонент | Роль | Страница |
| --- | --- | --- |
| Envoy Gateway `edge` | единственный публичный listener; делит один origin по путям | эта страница |
| Keycloak (realm `ai-stack`) | вход, роли `ai-user` / `ai-admin` | [Open WebUI](openwebui.ru.md), [PAT-сервис](pat-service.ru.md) |
| Open WebUI | чат в браузере, выбор модели, MCP-инструменты на уровне чата | [Open WebUI](openwebui.ru.md) |
| pat-service | дашборд PAT `/platform`, прокси `/v1`, MCP-прокси `/mcp/<name>/`, ключ сессии и метка приоритета | [PAT-сервис](pat-service.ru.md) |
| Envoy AI Gateway (`ai-gateway-private`) | внутрикластерная OpenAI-маршрутизация по имени модели, rate limit на пользователя, GenAI-трассировка | [Инференс](inference/README.ru.md) |
| llm-d EPP | очередь, полосы приоритета, справедливость между пользователями, скоринг эндпоинтов | [Очереди и fair share](inference/queues-and-fair-share.ru.md) |
| vLLM / SGLang / ninfer | движок модели; GPU владеет ровно один | [Движки](engines/README.ru.md) |
| эмбеддинги bge-m3 | `/v1/embeddings`, режим CPU или "заём" GPU | [Движки](engines/README.ru.md) |
| agent-broker и поды агентов | персональные агенты Hermes, pi и opencode под gVisor | [Агенты](agents/README.ru.md) |
| web-search, repowise | MCP-серверы за pat-service | [MCP](mcp/README.ru.md) |
| OTEL Collector, Langfuse | трейсы с промптами, ответами, пользователем и сессией | [Langfuse](observability/langfuse.ru.md) |
| Prometheus, Grafana | метрики и дашборды | [Наблюдаемость](observability/README.ru.md) |
| CloudNativePG, ClickHouse, Valkey, MinIO | состояние, каждым управляет свой оператор | [Данные и сроки хранения](observability/data-and-retention.ru.md) |

## Namespace-ы

| Namespace | Что внутри |
| --- | --- |
| `airgap-ai-stack` | все прикладные нагрузки: маршруты шлюза, Keycloak, Open WebUI, pat-service, EPP, движки, эмбеддинги, Langfuse, Prometheus, MCP-серверы |
| `agents` | `agent-broker` и персональные поды, PVC и Secret-ы агентов |
| `envoy-gateway-system`, `envoy-ai-gateway-system` | два контроллера шлюзов и прокси Envoy |
| `monitoring` | операторская Grafana (релиз `eg-addons`) с Loki, Tempo и своим Prometheus |
| `cnpg-system`, `clickhouse-operator`, `redis-operator`, `minio-operator` | операторы; их custom resource-ы живут в `airgap-ai-stack` |

## Три пути запроса

```
[Браузер]                                   [Программа с PAT]
  https://<origin>/                            https://<origin>/v1
    -> edge -> Open WebUI                        -> edge -> pat-service
         |  Keycloak-токен пользователя               |  PAT -> владелец, ключ сессии, полоса
         v                                            v
       ai-gateway-private  <--------------------------+
         |  проверка JWT, rate limit на пользователя, маршрут по имени модели
         v
       llm-d EPP (очередь, полосы, справедливость)  -> vLLM или SGLang
       (или, при INFERENCE_ENGINE=ninfer, напрямую в ninfer)

[Чат с агентом в Open WebUI]
  Open WebUI -> agent-broker (ns agents) -> под агента <runtime>-agent-<id> (gVisor)
                                              |  PAT агента этого пользователя
                                              v
                                           pat-service /v1 и /mcp/<name>/  -> как выше
```

- **Браузер.** Open WebUI вызывает приватный AI Gateway напрямую с
  Keycloak-токеном вошедшего пользователя. Через pat-service такой запрос не
  идёт, поэтому полосы приоритета у него нет и в EPP он попадает в самую
  нижнюю полосу `0` (см. [очереди](inference/queues-and-fair-share.ru.md)).
- **API.** `/v1` принимает только PAT вида `sk-...`. pat-service определяет
  владельца, вычисляет ключ сессии и полосу и вызывает шлюз со своим
  сервисным JWT и заголовками идентичности.
- **Агенты.** Open WebUI показывает `hermes-agent`, `pi-agent` и
  `opencode-agent` из третьего подключения к `agent-broker`. Брокер запускает
  под агента пользователя по требованию; агент вызывает модель и
  MCP-серверы через pat-service с собственным PAT пользователя.

У Grafana и Langfuse публичного маршрута нет: операторы открывают их через
NodePort-ы в сети хоста ([наблюдаемость](observability/README.ru.md)).
`/sso/admin` и `/sso/realms/master` на edge отвечают 404; администрирование
Keycloak идёт через `kubectl port-forward svc/keycloak`.

## Идентичность в запросе

Любой путь заканчивается одинаковыми доверенными заголовками в запросе к модели:

| Заголовок | Кто ставит | Для чего |
| --- | --- | --- |
| `X-User-Id` | Open WebUI (из JWT) или pat-service (владелец PAT) | rate limit на пользователя, `user_subject` в Langfuse |
| `X-User-Name` | так же | пользователь в Langfuse |
| `X-Pat-Token-Id` | pat-service | метаданные Langfuse: какой PAT |
| `X-Session-Key` | pat-service | сессия в Langfuse, "прогретость" в EPP ([PAT-сервис](pat-service.ru.md#склейка-сессий)) |
| `X-Llm-D-Inference-Objective` | pat-service | полоса приоритета EPP |
| `X-Llm-D-Inference-Fairness-Id` | pat-service | справедливость между пользователями в EPP |

Копии этих заголовков от клиента срезаются на edge и ещё раз в pat-service.
Сам по себе заголовок ничего не утверждает: шлюз проверяет JWT, который идёт
вместе с ним. Подробности и логика безопасности - в
[docs/security](../security/README.md).

## Кто чем владеет в репозитории

У каждого объекта Kubernetes ровно один владелец
([ADR 0002](../adr/0002-one-owner-per-object.md)):

| Путь | Владеет | Чем применяется |
| --- | --- | --- |
| `k8s/base` | нагрузки, не зависящие от профиля | рендерится в `helm/airgap-stack` скриптом `scripts/helm-render` |
| `k8s/overlays/remote-wsl-vllm-nvfp4` | GPU-предпосылки и патчи площадки удалённого профиля | тем же рендером; GPU-объекты - `make gpu-objects-up` |
| `helm/airgap-stack` | маршрутизация, маршруты AI Gateway, rate limit-ы, отрендеренные нагрузки | `make helm-up` |
| `helm/vllm-inference`, `helm/sglang-inference`, `helm/ninfer-inference`, `helm/embeddings-inference` | по одному движку | `make engine-up`, `make embeddings-up` |
| `config/llmd/router-nvfp4-values.yaml` | релиз llm-d EPP | `make llmd-up` |
| `k8s/agents`, `config/agents` | agent-broker и каталог агентов | `make agents-up`, `make agent-catalog` |
| `helm/web-search`, `helm/repowise` | MCP-серверы | `make websearch-up`, `make repowise-up` |
| `versions.lock.env` | все закреплённые версии образов и чартов | читается Makefile |
| `.env` (не коммитится) | секреты и значения площадки | `make helm-up` и соседние цели |

`make verify` рендерит и проверяет всё это без кластера и падает, если
закоммиченный чарт разошёлся с исходниками. Запускайте его перед каждым
коммитом, который трогает манифесты. `make stack-up` - единственный путь
полного деплоя удалённого профиля; пошаговая установка -
[docs/install](../install/README.md).

## Где это лежит

- Архитектурный справочник: [docs/architecture](../architecture/README.md)
- Границы безопасности: [docs/security](../security/README.md)
- Правила репозитория для людей и агентов: [AGENTS.md](../../AGENTS.md)
- Решения: [docs/adr](../adr/README.md)

## Связанные страницы

- Дальше: [Open WebUI](openwebui.ru.md)
- [Оглавление справочника](README.ru.md)
