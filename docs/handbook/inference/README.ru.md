# Инференс

[English](README.md) | [Оглавление справочника](../README.ru.md)

Как запрос к модели проходит от шлюза до GPU и кто по дороге что решает.
Дальше две более глубокие страницы: [очереди и fair share](queues-and-fair-share.ru.md)
(кто ждёт и в каком порядке) и [KV-кэш](kv-cache.ru.md) (что лежит в памяти
GPU и как размен контекста на параллелизм).

**Содержание**

- [Путь запроса](#путь-запроса)
- [Кто что решает](#кто-что-решает)
- [Как устроены движки](#как-устроены-движки)
- [Пути в обход EPP](#пути-в-обход-epp)
- [Таймауты и лимиты на пути](#таймауты-и-лимиты-на-пути)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Путь запроса

```
клиент (Open WebUI | pat-service)
  -> ai-gateway-private (Envoy AI Gateway, envoy-gateway-system)
       SecurityPolicy llmd-jwt: нужен JWT Keycloak
       BackendTrafficPolicy llmd-per-user-limit: запросов в минуту на X-User-Id
       AIGatewayRoute llmd: правило по имени модели (x-ai-eg-model)
  -> AIServiceBackend llmd-qwen-test-openai
  -> под llm-d EPP (llmd-qwen-test-epp): sidecar Envoy + ext-proc
       flow control: очередь по полосе и по пользователю, отправка, пока нет насыщения
       scheduling: скоринг эндпоинтов, выбор одного
  -> под движка (vLLM или SGLang, :8000), OpenAI API
```

Имя модели, которое шлют клиенты, `qwen-3.8-27b` (`inference.modelName`),
обозначает пул, а не движок. Шлюз маршрутизирует по этому имени; EPP
отправляет запросы в любой под vLLM или SGLang с меткой
`llm-d.ai/model=qwen-3.8-27b` ([движки](../engines/README.ru.md#реплики-движков)).

AI Gateway также создаёт по одному GenAI-спану на запрос (промпт, ответ,
модель, токены, пользователь, сессия), который попадает в Langfuse
([наблюдаемость](../observability/langfuse.ru.md)).

## Кто что решает

| Решение | Владелец | Где настраивается |
| --- | --- | --- |
| Пускать ли вызывающего вообще | AI Gateway (JWT), pat-service (PAT) | `helm/airgap-stack/templates/llmd.yaml`, Keycloak |
| Сколько запросов в минуту на пользователя | rate limit AI Gateway | `inference.perUserRateLimitPerMinute` |
| Какие полоса и fairness id у запроса | pat-service (только метка) | env `QOS_*` |
| Когда запрос отправляется и в каком порядке | flow control llm-d EPP | `config/llmd/router-nvfp4-values.yaml` `flowControl` |
| Какому поду движка он достаётся | планировщик llm-d EPP | тот же файл, `schedulingProfiles`, `modelServers` |
| Сколько последовательностей одновременно на GPU, длина контекста, память KV | движок | `helm/<engine>-inference/values.yaml` |

pat-service никогда не придерживает запрос; см.
[PAT-сервис: чего не делает](../pat-service.ru.md#чего-не-делает).

## Как устроены движки

- **Один GPU - один движок.** vLLM, SGLang и ninfer запрашивают
  `nvidia.com/gpu: 1` со `strategy: Recreate`; второй движок остаётся в
  `Pending`, а не дерётся за карту. GPU делит с движком только сервер
  эмбеддингов - вне учёта планировщика, с фиксированным бюджетом памяти.
- **Каждый движок - свой Helm-релиз** (`helm/vllm-inference`,
  `helm/sglang-inference`, `helm/ninfer-inference`), поэтому смена флага
  запуска перезапускает только его под ([ADR 0007](../../adr/0007-inference-engines-as-helm-releases.md)).
- **Веса лежат на read-only host-path PV** (`/var/lib/models` в узле k3d),
  общем для всех движков; в образах весов нет.
- **Одна реплика.** Скореры EPP выбирают между эндпоинтами; при одном поде
  выбирать не из чего, поэтому на этой площадке ценность EPP - очередь, а не
  маршрутизация. Скореры заработают, когда появится вторая реплика или GPU.
- **Параллелизм определяет движок.** vLLM держит `inference.maxNumSeqs` (2)
  последовательности одновременно, SGLang - `inference.maxRunningRequests`
  (3). Всё сверх этого ждёт: сначала внутри движка, а когда EPP видит
  насыщение - в очереди EPP, где действуют полосы и справедливость.

Подробности о движках, переключении и подключении новых: [движки](../engines/README.ru.md).

## Пути в обход EPP

| Трафик | Почему | Следствие |
| --- | --- | --- |
| `qwen-3.8-27b-ninfer` | у ninfer нет `/metrics`, которые мог бы читать EPP ([ADR 0015](../../adr/0015-third-party-engine-metrics-contract.md)) | своё правило указывает прямо на ninfer: ни полос, ни справедливости, только rate limit на пользователя |
| `llamacpp-local`, `external-api` (если включены) | свои имена моделей и правила маршрута ([ADR 0006](../../adr/0006-pluggable-inference-backends.md)) | то же: очереди перед ними нет |
| эмбеддинги `bge-m3` | отдельный маршрут `embeddings`, свой rate limit | в общую очередь с чатом не попадают |

## Таймауты и лимиты на пути

| Где | Значение | Где задаётся |
| --- | --- | --- |
| Таймаут запроса на edge `/v1` | 10 мин | `helm/airgap-stack/templates/routes-external.yaml` |
| Таймаут запроса и простоя стрима в правиле AI Gateway | 10 мин | `helm/airgap-stack/templates/llmd.yaml` |
| TTL очереди EPP | 10 мин (`flowControl.defaultRequestTTL`) | `config/llmd/router-nvfp4-values.yaml` |
| Буфер тела запроса на маршруте модели | 4 MiB | `llmd.yaml` `llmd-per-user-limit` |
| Rate limit на пользователя | 60 / мин | `inference.perUserRateLimitPerMinute` |
| Лимит на IP для `/v1` на edge | 60 / мин | `routes-external.yaml` `edge-api-ip-rate-limit` |

Как менять их по метрикам: [тюнинг](../configuration/tuning.ru.md).

## Где это лежит

- Маршруты и политики шлюза: `helm/airgap-stack/templates/llmd.yaml`,
  `inference-backends.yaml`, `embeddings.yaml`; values `inference.*`
- EPP: `config/llmd/router-nvfp4-values.yaml`, `make llmd-up`
- Движки: `helm/*-inference/values.yaml`, `make engines-up`
- Runbook-и: [inference-backends.md](../../operations/inference-backends.md),
  [vllm-inference.md](../../operations/vllm-inference.md)

## Связанные страницы

- Назад: [PAT-сервис](../pat-service.ru.md). Дальше: [Очереди и fair share](queues-and-fair-share.ru.md)
- [ADR 0008](../../adr/0008-per-user-fair-share.md) fair share,
  [ADR 0012](../../adr/0012-graduated-band-ceiling-over-strict-priority.md)
  потолок полос, [ADR 0010](../../adr/0010-sglang-hicache-kv-offload.md) выгрузка KV
