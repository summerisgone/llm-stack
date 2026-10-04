# Движки

[English](README.md) | [Оглавление справочника](../README.ru.md)

Движок - процесс, который выполняет модель на GPU. vLLM и SGLang -
"граждане первого класса": их поды входят в пул `qwen-3.8-27b` за llm-d EPP
и получают очередь и fair share. ninfer не может войти в EPP и обслуживает
своё имя модели, `qwen-3.8-27b-ninfer`. Strata работает на GPU-хосте и
обслуживает `qwen-3.8-flash-next` через слот внешнего API. llama.cpp и внешние
OpenAI-совместимые API - тоже отдельные имена моделей. Имя модели `default`
всегда ведёт на движок, который обслуживает сейчас. Эта страница про
пул, реплики движков и одну команду, которая их применяет; остальное - в
[подключении движка](adding-an-engine.ru.md).

**Содержание**

- [Матрица движков](#матрица-движков)
- [Как подключён движок первого класса](#как-подключён-движок-первого-класса)
- [Реплики движков](#реплики-движков)
- [Имя модели `default`](#имя-модели-default)
- [Изменение настроек движка](#изменение-настроек-движка)
- [Эмбеддинги](#эмбеддинги)
- [Проверки после изменения](#проверки-после-изменения)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Матрица движков

| Движок | Имя модели | Релиз, команда | За EPP | Метрики | Статус |
| --- | --- | --- | --- | --- | --- |
| vLLM 0.27.1 | `qwen-3.8-27b` | `helm/vllm-inference`, `make vllm-up` | да | нативные `/metrics` (`vllm:*`) | по умолчанию |
| SGLang 0.5.19 | `qwen-3.8-27b` | `helm/sglang-inference`, `make sglang-up` | да | нативные `/metrics` (`sglang:*`) | альтернатива первого класса |
| ninfer | `qwen-3.8-27b-ninfer` | `helm/ninfer-inference`, `make ninfer-up` | нет, прямой маршрут | JSONL-лог, переэкспортированный sidecar-ом (`ninfer_*`) | пилот |
| Strata (Qwen3.8-Flash-Next IQ3_S) | `qwen-3.8-flash-next` | Docker на хосте, `deploy/strata/run`; маршрут `inference.externalApi` | нет, прямой маршрут | нет в Prometheus (свой JSON `/metrics` на хосте) | включён, на хосте |
| Strata в кластере | `qwen-3.8-flash-next` | `helm/strata-inference`, `make strata-up` | нет, прямой маршрут | JSON `/metrics`, переэкспортированный sidecar-ом (`strata_*`) | выключен: мало RAM на этой ноде |
| llama.cpp | `llamacpp-local` | Docker на хосте, `deploy/llamacpp` | нет | нет | опционально, выключен |
| внешний API | `external-api` | не управляется стеком | нет | нет | занят Strata |
| bge-m3 (эмбеддинги) | `bge-m3` | `helm/embeddings-inference`, `make embeddings-up` | нет, свой маршрут | нативные | включён |

Каждый под движка запрашивает `nvidia.com/gpu: 1` и работает на GPU-ноде
(`node-role/inference=true`,
[ADR 0019](../../adr/0019-inference-plane-gpu-worker-nodes.md)). При одной
GPU одновременно работает один под движка. Strata на хосте занимает ту же
GPU в обход Kubernetes: пока он работает, все реплики движков остаются 0
([deploy/strata](../../../deploy/strata/README.md)).

## Как подключён движок первого класса

```
AIGatewayRoute llmd, правило openai-qwen38nvfp4 (model == qwen-3.8-27b)
  -> AIServiceBackend llmd-qwen-test-openai            (никогда не сам движок)
  -> EPP: селектор InferencePool llm-d.ai/model=qwen-3.8-27b, порт 8000
  -> любой под vLLM или SGLang с этой меткой, llm-d.ai/engine-type=vllm|sglang
```

Под становится членом пула по двум меткам, обе ставит чарт движка:

1. **`llm-d.ai/model`** (`inference.servedModelName` чарта): по ней выбирает
   `router.modelServers.matchLabels` EPP в
   `config/llmd/router-nvfp4-values.yaml`, порт 8000 у всех движков.
2. **`llm-d.ai/engine-type`**: `core-metrics-extractor` EPP по ней узнаёт
   имена метрик очереди и KV. Без неё EPP считает движок vLLM, метрики SGLang
   не разбираются и эндпоинт устаревает. Поэтому поды vLLM и SGLang могут
   быть в одном пуле.

Движки работают без API-ключа. NetworkPolicy `inference-pool-members`
(`helm/airgap-stack/templates/llmd.yaml`) пускает на порт 8000 подов с
`llm-d.ai/model` только под EPP и Prometheus.

ninfer не входит в пул: правило `openai-ninfer` (model ==
`qwen-3.8-27b-ninfer`) ведёт прямо в его `AIServiceBackend`, ключ API
подставляет шлюз, очереди и fair share EPP нет.

## Реплики движков

Мощность - число реплик каждого движка, задаётся в `.env`:

```sh
# .env
VLLM_REPLICAS=0
SGLANG_REPLICAS=1
NINFER_REPLICAS=0
STRATA_REPLICAS=0
STRATA_ON_HOST=0   # 1, пока Strata работает на хосте и занимает GPU
```

```sh
make engines-up
```

`engines-up` выполняет по порядку:

1. масштабирует под эмбеддингов на GPU в 0 (LLM-движок стартует первым,
   [ADR 0013](../../adr/0013-embeddings-api-bge-m3.md));
2. применяет движки, которые уходят в 0, и ждёт удаления их подов;
3. применяет остальные через `make <engine>-up` и ждёт Ready (загрузка
   модели занимает несколько минут);
4. `make llmd-up`;
5. `make embeddings-up` и возвращает эмбеддинги в 1.

`make <engine>-up` отдельно применяет values движка с числом реплик из
`.env`. При одной GPU держите сумму равной 1: `qwen-3.8-27b` отвечает, пока
у vLLM или SGLang есть реплика, `qwen-3.8-27b-ninfer` - пока она есть у
ninfer. Перенос GPU между vLLM и SGLang для клиентов незаметен; перенос на
ninfer или Strata оставляет `qwen-3.8-27b` без эндпоинтов. Клиенты на
`default` переходят вслед за движком после `make helm-up`; клиентов на
собственном имени движка нужно предупредить. При нескольких GPU реплики vLLM
и SGLang складываются в одном пуле.

## Имя модели `default`

`default` (`inference.defaultModel` в `helm/airgap-stack/values.yaml`) - имя
модели, не привязанное к движку. Шлюз отправляет его на текущий движок и
подменяет `model` в запросе на собственное имя этого движка
(`modelNameOverride`), поэтому клиент один раз ставит `model: default` и не
меняет его при смене движка. Правило catch-all идёт туда же: запрос с
неизвестным именем модели попадает на тот же движок.

| Реплики движков в `.env` | Куда идёт `default` |
| --- | --- |
| `VLLM_REPLICAS` или `SGLANG_REPLICAS` больше 0 | пул `qwen-3.8-27b` через EPP |
| иначе `NINFER_REPLICAS` больше 0 | ninfer, `qwen-3.8-27b-ninfer` |
| иначе `STRATA_REPLICAS` больше 0 | Strata в кластере |
| иначе `STRATA_ON_HOST=1` | Strata на хосте, через `inference.externalApi` |

Makefile выводит `DEFAULT_ENGINE` по этой таблице; чтобы переопределить,
задайте его в `.env` (`pool`, `ninfer`, `strata`, `externalApi`,
`llamacpp`). Маршрут меняется на `make helm-up`, а не на `engines-up`; тот
только печатает, куда ведёт `default`. Смена движка поэтому такая:

```sh
# .env: новые *_REPLICAS
make engines-up
make helm-up      # переводит `default` (и catch-all) на новый движок
```

Модель за `default` меняется вместе с движком; её называет поле `model` в
ответе. Клиент, которому нужно поведение конкретной модели (JSON mode,
который ninfer отклоняет; Qwen3.8-27B, а не Flash-Next), использует её
собственное имя. Облачные агенты (`config/agents/base-profile`) и список
моделей Open WebUI используют `default`; агенты рассчитывают на контекст
131 072 токена, наименьший среди движков.

## Изменение настроек движка

Флаги запуска - это values, а не манифесты: `helm/vllm-inference/values.yaml`
(`maxModelLen`, `maxNumSeqs`, `gpuMemoryUtilization`, ...) и
`helm/sglang-inference/values.yaml` (`contextLength`, `maxRunningRequests`,
`memFractionStatic`, ...). Применяйте `make vllm-up` / `make sglang-up`, пока
у этого движка есть реплики; перезапускаются только его поды. Что делает каждое
значение и как они соотносятся: [KV-кэш](../inference/kv-cache.ru.md).
Если включены эмбеддинги на GPU, перезапускайте их после движка
(`kubectl -n airgap-ai-stack rollout restart deploy/embeddings-bge-m3`).

`make verify` не проверяет чарты движков; рендерите их вручную:
`helm template x helm/vllm-inference >/dev/null`.

## Эмбеддинги

`bge-m3` обслуживает `/v1/embeddings` через свой маршрут и свой rate limit
(`inference.embeddings.*`, 120 запросов в минуту на пользователя). В режиме
`accelerator: gpu` (`helm/embeddings-inference/values.yaml`) он занимает
`gpu.gpuMemoryBudgetGiB` памяти GPU вне учёта GPU в Kubernetes, поэтому доля
памяти LLM-движка должна оставлять столько свободным, а порядок старта
фиксирован: сначала движок. У `accelerator: cpu` таких ограничений нет.
repowise нужен режим GPU ([ADR 0013](../../adr/0013-embeddings-api-bge-m3.md),
[ADR 0018](../../adr/0018-repowise-codebase-intelligence.md)).

Бюджет 3 GiB измерен при ninfer в роли живого движка (пик эмбеддингов около
2.2 GiB). vLLM при `gpuMemoryUtilization: 0.94` берёт 29.9 из 31.8 GiB
карты, оставляя около 1.9 GiB: оба стартуют и отвечают (проверено), но
тяжёлая нагрузка на эмбеддинги под vLLM может исчерпать память карты.
Перед такой нагрузкой уменьшите `gpuMemoryUtilization` или используйте
`accelerator: cpu`, пока живой vLLM.

## Проверки после изменения

```sh
kubectl -n airgap-ai-stack get pods -l 'app.kubernetes.io/name in (vllm-qwen38-nvfp4,sglang-qwen38,ninfer-qwen38,embeddings-bge-m3)' -L llm-d.ai/engine-type
kubectl -n airgap-ai-stack get aigatewayroute llmd -o jsonpath='{range .spec.rules[*]}{.name}{" -> "}{.backendRefs[0].name}{"\n"}{end}'
make llmd-nvfp4-smoke      # выпуск PAT, инференс, rate limit, отзыв (нужен STACK_BASE_URL)
```

В `:9090/metrics` EPP `llm_d_epp_ready_endpoints` равен сумме реплик vLLM и
SGLang (0, пока работает только ninfer, которого EPP не обслуживает), а
`llm_d_epp_flow_control_stale_endpoints` остаётся 0.

## Где это лежит

- Чарты: `helm/vllm-inference`, `helm/sglang-inference`,
  `helm/ninfer-inference`, `helm/embeddings-inference`
- Реплики: `Makefile` (`VLLM_REPLICAS`, `SGLANG_REPLICAS`,
  `NINFER_REPLICAS`, `engines-up`)
- Правила шлюза: `helm/airgap-stack/templates/llmd.yaml`,
  `inference-backends.yaml`
- Образы: `versions.lock.env`; образ vLLM собирается на GPU-хосте
  (`deploy/vllm-qwen38-nvfp4`)
- Runbook-и: [inference-backends.md](../../operations/inference-backends.md),
  [vllm-inference.md](../../operations/vllm-inference.md),
  [deploy/sglang-qwen38](../../../deploy/sglang-qwen38/README.md)

## Связанные страницы

- Назад: [KV-кэш](../inference/kv-cache.ru.md). Дальше: [Подключение движка](adding-an-engine.ru.md)
- [ADR 0006](../../adr/0006-pluggable-inference-backends.md),
  [ADR 0007](../../adr/0007-inference-engines-as-helm-releases.md),
  [ADR 0015](../../adr/0015-third-party-engine-metrics-contract.md)
