# Трейсы Langfuse

[English](langfuse.md) | [Оглавление справочника](../README.ru.md)

В Langfuse лежит по трейсу на каждый вызов модели через приватный AI
Gateway: промпт, ответ, модель, число токенов, задержка и кто спрашивал.
Это место, где отвечают на вопрос "что именно отправил этот пользователь
или агент и что пришло обратно". Поскольку здесь хранится содержимое
разговоров, доступ к Langfuse - это доступ к пользовательским данным.

**Содержание**

- [Откуда берутся трейсы](#откуда-берутся-трейсы)
- [Что в трейсе](#что-в-трейсе)
- [Пользователи и сессии](#пользователи-и-сессии)
- [Как читать трейсы](#как-читать-трейсы)
- [Что не трассируется](#что-не-трассируется)
- [Диагностика](#диагностика)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Откуда берутся трейсы

```
ai-gateway-private
  трассировка EnvoyProxy (service ai-gateway-private): HTTP-спаны, 100 % выборка  --\
  GatewayConfig ai-gateway-tracing (service ai-gateway-genai): GenAI-спаны        ---> OTEL Collector
                                                                                       фильтр: отбросить service.name == ai-gateway-private
                                                                                       экспорт: Langfuse /api/public/otel (basic auth)
```

- GenAI-спаны создаёт external processor AI Gateway по конвенциям
  OpenInference, `always_on`, с `OPENINFERENCE_HIDE_INPUTS/OUTPUTS=false`:
  входы и выходы записываются, включая стриминг.
- Collector отбрасывает собственные транспортные спаны Envoy, чтобы каждый
  запрос появлялся один раз, и аутентифицируется в Langfuse через
  `LANGFUSE_PUBLIC_KEY` / `LANGFUSE_SECRET_KEY` из `.env`. Envoy эти ключи не
  видит.
- `tests/telemetry/genai-smoke.sh` проверяет всю цепочку: стриминговый и
  нестриминговый вызов должны попасть в ClickHouse с входом, выходом,
  моделью и пользователем, и ни один HTTP-спан Envoy не должен быть
  экспортирован.

## Что в трейсе

| Поле Langfuse | Источник |
| --- | --- |
| Input, output | `input.value`, `output.value` (OpenInference) |
| Model | `llm.model_name` (`qwen-3.8-27b`, либо `llamacpp-local` и т.п.) |
| Токены | `llm.token_count.prompt` / `completion` / `total` |
| Задержка | длительность спана |
| User | `X-User-Name` -> `langfuse.user.id` |
| Метаданные `user_subject` | `X-User-Id` (Keycloak `sub`) |
| Метаданные `pat_token_id` | `X-Pat-Token-Id` (только PAT-трафик) |
| Session | `X-Session-Key` или `agent-session-id` -> `session.id` |

Отображение заголовков в атрибуты - одна строка в
`config/ai-gateway/values.yaml` (`controller.spanRequestHeaderAttributes`),
применяется релизом `aieg` (`make gateway-up`); настройки трассировки
конкретного шлюза - в `helm/airgap-stack/templates/llmd.yaml`.

## Пользователи и сессии

- **Чаты из браузера** (Open WebUI) несут пользователя из пересылаемых Open
  WebUI заголовков, но без сессии: каждый вызов - отдельный трейс.
- **PAT-трафик и агенты** несут пользователя и ключ сессии pat-service, так
  что все шаги одного запуска агента складываются в одну сессию Langfuse с
  суммарными стоимостью, токенами и задержкой
  ([склейка сессий](../pat-service.ru.md#склейка-сессий)).
- Агенты ходят с собственным PAT агента пользователя, поэтому их трейсы
  приписаны пользователю, с `pat_token_id` ключа агента.
- Токены, выпущенные до того, как стали записываться имена пользователей,
  показывают `sub` вместо имени; новый PAT исправляет последующие трейсы.

## Как читать трейсы

| Картина | Что значит | Что дальше |
| --- | --- | --- |
| Большие `prompt_tokens`, маленькие `completion_tokens`, повторяются | агент пересылает историю | посмотреть попадания в префиксный кэш в Grafana; долю полосы `warm` в QoS / Fair share |
| Задержка растёт, токены - нет | ожидание в очереди EPP или движка | QoS / Cluster load, длительность очереди EPP |
| Много коротких сессий от одного PAT | клиент не пересылает историю или редактирует её | для некоторых инструментов это нормально; сессии определяются префиксом разговора |
| Ошибки с `response_format` | JSON-режим при живом ninfer | [движки](../engines/README.ru.md) |

## Что не трассируется

- Вызовы, которые не проходят через `ai-gateway-private`: собственные ответы
  онбординга agent-broker, вызовы MCP-инструментов (вместо этого они
  считаются в `patsvc_mcp_calls_total`), эмбеддинги (отдельный маршрут;
  число токенов только в pat-service).
- Трафик, отправленный в движок напрямую (port-forward, тесты изнутри пода).

## Диагностика

| Симптом | Проверить |
| --- | --- |
| Нет новых трейсов | `kubectl -n airgap-ai-stack logs deploy/otel-collector`; ключи Langfuse в `.env` совпадают с проектом (Settings -> API keys) |
| Трейсы без входа и выхода | не применён `GatewayConfig`: `make helm-up`; старые трейсы не восстановить |
| Дублирующиеся трейсы | пропал фильтр Collector для спанов `ai-gateway-private` |
| Нет сессий | ошибки Valkey в pat-service или потерян маппинг `x-session-key` в `config/ai-gateway/values.yaml` |

## Где это лежит

- Langfuse web и worker: `k8s/base/applications.yaml`; хранилища в
  `k8s/base/databases.yaml` (Postgres `langfuse-db`, ClickHouse
  `langfuse-clickhouse`, Valkey `langfuse-valkey`) и
  `k8s/base/object-storage.yaml` (bucket MinIO `langfuse`)
- Collector: `k8s/base/observability.yaml`
- Настройки трассировки: `helm/airgap-stack/templates/llmd.yaml`,
  `config/ai-gateway/values.yaml`
- Подробнее: [README эксплуатации](../../operations/README.md#ai-gateway-traces-in-langfuse),
  [config/langfuse](../../../config/langfuse/README.md)

## Связанные страницы

- Назад: [Наблюдаемость](README.ru.md). Дальше: [Данные и сроки хранения](data-and-retention.ru.md)
- [ADR 0011](../../adr/0011-pat-session-key-langfuse-tracing.md)
