# KV-кэш

[English](kv-cache.md) | [Оглавление справочника](../README.ru.md)

KV-кэш - это память GPU, в которой лежит состояние внимания каждого идущего
разговора. Его размер определяет, насколько длинным может быть контекст,
сколько последовательностей работает одновременно и как часто вернувшийся
разговор получает свой префикс бесплатно. Здесь описаны рычаги каждого
движка и то, как остальной стек (EPP, полосы pat-service) использует кэш.

**Содержание**

- [Почему это здесь важно](#почему-это-здесь-важно)
- [Особенность модели](#особенность-модели)
- [vLLM](#vllm)
- [SGLang](#sglang)
- [ninfer](#ninfer)
- [Как стек использует кэш](#как-стек-использует-кэш)
- [Контекст против параллелизма](#контекст-против-параллелизма)
- [За чем следить](#за-чем-следить)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Почему это здесь важно

Одна карта на 32 GB вмещает веса 27B-модели, а всё оставшееся становится
пулом KV. vLLM при `gpuMemoryUtilization: 0.94` пишет в лог 19.1 GiB на веса
и 7.6 GiB KV-кэша - это 230 104 токена в FP8. Coding-агенты шлют длинные,
растущие промпты: каждый шаг пересылает всю историю. Отсюда два следствия:

- **Главное ускорение - переиспользование префикса.** Если префикс
  предыдущего шага ещё в кэше, считается только новый хвост. Поэтому
  pat-service помечает сессию `warm` на 120 с после последней отправки, а EPP
  обслуживает `warm` первой ([очереди](queues-and-fair-share.ru.md)).
- **Узкое место - пул, а не вычисления.** Сигнал насыщения EPP считает
  занятость KV в 80 % насыщением.

## Особенность модели

`qwen-3.8-27b` (архитектура `Qwen3_5ForConditionalGeneration`) - гибрид:
16 слоёв полного внимания и 48 слоёв линейного внимания (в стиле Mamba).
Поэтому переиспользуемый префикс состоит из двух частей, attention KV и
состояния Mamba, а часть Mamba движки кэшируют по-разному и менее зрело.
Большинство сюрпризов ниже - отсюда.

## vLLM

Значения в разделе `inference` файла `helm/vllm-inference/values.yaml`:

| Значение | Настройка | Почему |
| --- | --- | --- |
| `gpuMemoryUtilization` | `0.94` | 0.95 на этой карте не стартует (свободно бывает около 30.2 GiB); 0.94 - на шаг ниже |
| `maxModelLen` | `131072` | контекст одного запроса; vLLM требует, чтобы влезала только одна последовательность полной длины |
| `maxNumSeqs` | `2` | одновременно декодируемые последовательности: потолок допуска |
| `maxNumBatchedTokens` | `8192` | порция prefill за шаг планировщика |
| `kvCacheDtype` | `fp8` | вдвое меньше памяти KV, чем в BF16 |
| `prefixCaching` | `true` | переиспользование одинаковых префиксов; для этой модели vLLM сам переводит кэш Mamba в режим `align` |
| `kvOffloadingSizeGiB` / `kvOffloadingBackend` | `8` / `native` | вытесненные блоки KV уходят в буфер `/dev/shm`, и вытесненная сессия подгружается, а не пересчитывается; `dshmSizeGiB` (12) должен быть больше |

При 131072 пул вмещает около 230 K токенов, то есть 1.76 контекста полной
длины: двум сессиям максимальной длины одновременно понадобилось бы примерно
на 12 % больше, и это гасит CPU-выгрузка через вытеснение и подгрузку.
Выгрузка **не** позволяет одной последовательности превысить пул GPU.
Замеры и история тюнинга - в [vllm-inference.md](../../operations/vllm-inference.md).

## SGLang

Значения в разделе `inference` файла `helm/sglang-inference/values.yaml`:

| Значение | Настройка | Почему |
| --- | --- | --- |
| `contextLength` | `180224` | контекст одного запроса; отдаётся и как `max_model_len` в `/v1/models` |
| `memFractionStatic` | `"0.90"` | доля памяти GPU под веса и пул KV |
| `maxRunningRequests` | `3` | потолок допуска |
| `maxMambaCacheSize` | `12` | слоты состояния Mamba |
| `mambaRadixCacheStrategy` | `extra_buffer_lazy` | как состояние Mamba участвует в radix (префиксном) кэше |
| `chunkedPrefillSize` | `2048` | порция prefill |
| `kvCacheDtype` | `auto` | тип данных модели |

KV остаётся на GPU. HiCache (уровень в RAM хоста) и CPU-выгрузка весов на
этом гибридном чекпойнте с SGLang 0.5.19 на RTX 5090 падают; ADR 0010 держит
их выключенными, пока исправление из upstream не пройдёт перечисленные там
тесты ([ADR 0010](../../adr/0010-sglang-hicache-kv-offload.md),
[исследование](../../operations/sglang-hicache-study-2026-09-07.md)).

## ninfer

У ninfer свой префиксный кэш (ответы содержат
`prompt_tokens_details.cached_tokens`), но нет Prometheus `/metrics`, которые
мог бы читать EPP, поэтому состояния его KV EPP не видит, и маршрут идёт в
обход EPP ([движки](../engines/adding-an-engine.ru.md#ninfer)). Настройки
памяти - в `helm/ninfer-inference/values.yaml`.

## Как стек использует кэш

| Компонент | Использует | Как |
| --- | --- | --- |
| pat-service | "прогретость" | полоса `warm` для сессий, отправленных в пределах `QOS_WARM_TTL_SECONDS` |
| flow control EPP | занятость | `utilization-detector`: насыщение при `kvCacheUtilThreshold` 0.8 |
| планировщик EPP | занятость, префиксы | `kv-cache-utilization-scorer`, `prefix-cache-scorer` (важны только при нескольких репликах) |
| эмбеддинги на GPU | остаток памяти | `gpu.gpuMemoryBudgetGiB` (3) в `helm/embeddings-inference` вырезается из того, что оставляет LLM-движок; увеличение означает уменьшение `gpuMemoryUtilization` / `memFractionStatic` в том же изменении ([ADR 0013](../../adr/0013-embeddings-api-bge-m3.md)) |

## Контекст против параллелизма

Пул задан картой; ручки только делят его:

- **Длиннее контекст** (`maxModelLen` / `contextLength`) - один запрос может
  быть больше, но одновременно влезает меньше полноразмерных; vLLM тогда
  вытесняет, SGLang ставит в очередь.
- **Больше слотов** (`maxNumSeqs` / `maxRunningRequests`) - выше пропускная
  способность на коротких промптах, а длинные начинают вытеснять друг друга.
- **Больше памяти под пул** (`gpuMemoryUtilization`, `memFractionStatic`) -
  ограничено тем, сколько карта реально отдаёт, и бюджетом эмбеддингов.

Меняйте одно значение, применяйте `make engine-up` (или `make <engine>-up`
самого движка, если он уже живой) и сравнивайте метрики ниже под реальной
нагрузкой. Процедура - в [тюнинге](../configuration/tuning.ru.md#ёмкость-движка).

## За чем следить

| Вопрос | vLLM | SGLang |
| --- | --- | --- |
| Занятость пула KV | `vllm:kv_cache_usage_perc` | `sglang:token_usage` |
| Попадания в префиксный кэш | `vllm:prefix_cache_hits_total` / `vllm:prefix_cache_queries_total` | `sglang:cache_hit_rate` |
| Запросы, ждущие в движке | `vllm:num_requests_waiting` | `sglang:num_queue_reqs` |
| Вытеснения, трафик выгрузки | `vllm:num_preemptions_total`, `vllm:kv_offload_total_bytes_total`, `vllm:kv_offload_cpu_cache_usage_perc` | - |
| Кэшированные токены промпта по пользователям | `patsvc_cached_prompt_tokens_total` остаётся 0: ни vLLM 0.27.1, ни SGLang 0.5.19 не отдают `cached_tokens` в `usage` (ninfer отдаёт) | то же |

Дашборды: vLLM / vLLM, llm-d / Performance and KV cache, QoS / Fair share для
SGLang ([наблюдаемость](../observability/README.ru.md)).

## Где это лежит

- `helm/vllm-inference/values.yaml`, `helm/sglang-inference/values.yaml`,
  `helm/ninfer-inference/values.yaml`
- Отображение в флаги: `helm/<engine>-inference/templates/inference.yaml`
- Замеры: [vllm-inference.md](../../operations/vllm-inference.md),
  `tests/inference/sglang-hicache-results-*.md`

## Связанные страницы

- Назад: [Очереди и fair share](queues-and-fair-share.ru.md). Дальше: [Движки](../engines/README.ru.md)
- [ADR 0010](../../adr/0010-sglang-hicache-kv-offload.md), [ADR 0013](../../adr/0013-embeddings-api-bge-m3.md)
