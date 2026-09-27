# Движки

[English](README.md) | [Оглавление справочника](../README.ru.md)

Движок - процесс, который выполняет модель на GPU. vLLM и SGLang -
"граждане первого класса": они стоят за llm-d EPP и получают очередь и fair
share. ninfer может занять то же имя модели, но идёт в обход EPP. llama.cpp
и внешние OpenAI-совместимые API - дополнительные имена моделей. Эта
страница про движки первого класса и одну команду, которая переключает все
три внутрикластерных движка; остальное - в [подключении движка](adding-an-engine.ru.md).

**Содержание**

- [Матрица движков](#матрица-движков)
- [Как подключён движок первого класса](#как-подключён-движок-первого-класса)
- [Переключение живого движка](#переключение-живого-движка)
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
| ninfer | `qwen-3.8-27b` | `helm/ninfer-inference`, `make ninfer-up` | нет, прямой маршрут | JSONL-лог, переэкспортированный sidecar-ом (`ninfer_*`) | пилот |
| llama.cpp | `llamacpp-local` | Docker на хосте, `deploy/llamacpp` | нет | нет | опционально, выключен |
| внешний API | `external-api` | не управляется стеком | нет | нет | опционально, выключен |
| bge-m3 (эмбеддинги) | `bge-m3` | `helm/embeddings-inference`, `make embeddings-up` | нет, свой маршрут | нативные | включён |

Все внутрикластерные движки монтируют один read-only PV с моделью и
запрашивают `nvidia.com/gpu: 1`, поэтому работать может только один.

## Как подключён движок первого класса

```
AIGatewayRoute llmd, правило openai-qwen38nvfp4 (model == qwen-3.8-27b)
  -> AIServiceBackend llmd-qwen-test-openai            (никогда не сам движок)
  -> EPP: селектор InferencePool app.kubernetes.io/name=<deployment движка>, порт 8000|30000
  -> под движка с меткой llm-d.ai/engine-type=vllm|sglang
```

Чтобы EPP отправлял запросы в движок, должны сойтись три вещи, и
`make engine-up` выставляет все три из одной переменной:

1. **Селектор и порт** в `router.modelServers` EPP (`LLMD_ENGINE_SETS` в
   Makefile, передаются в `make llmd-up`).
2. **Метка типа движка** на поде (`llm-d.ai/engine-type`, ставит чарт
   движка). По ней `core-metrics-extractor` EPP узнаёт имена метрик глубины
   очереди и занятости KV; без неё EPP считает движок vLLM-ом, метрики не
   разбираются, и эндпоинт становится stale.
3. **Ключ апстрима.** SGLang требует bearer-токен. При
   `inference.sglang.enabled=true` (его ставит `make helm-up`, когда
   `INFERENCE_ENGINE=sglang`) чарт airgap-stack создаёт Secret
   `sglang-api-key` из `SGLANG_API_KEY` и `BackendSecurityPolicy`, которая
   подставляет его на пути через EPP. vLLM ключ не нужен.

Само правило шлюза при переходе между vLLM и SGLang не меняется. Для ninfer
то же правило перенаправляется на его собственный backend
(`inference.ninfer.live=true`).

## Переключение живого движка

Живой движок - одна переменная в `.env`:

```sh
# .env
INFERENCE_ENGINE=sglang        # vllm | sglang | ninfer
```

```sh
make engine-up
```

`engine-up` выполняет по порядку:

1. масштабирует GPU-под эмбеддингов в 0 (LLM-движок должен стартовать
   первым, [ADR 0013](../../adr/0013-embeddings-api-bge-m3.md));
2. масштабирует остальные движки в 0 и ждёт, пока их поды исчезнут;
3. `make helm-up`: направляет `qwen-3.8-27b` (в EPP или сразу в ninfer) и
   создаёт Secret с API-ключом движка;
4. `make <engine>-up` и ждёт готовности движка (загрузка модели занимает
   несколько минут);
5. `make llmd-up`: направляет EPP на движок;
6. `make embeddings-up` и масштабирует эмбеддинги обратно в 1.

Клиенты продолжают слать `qwen-3.8-27b`; во время переключения запросы
падают, так что предупредите пользователей заранее. `helm-up` и `llmd-up`
читают `INFERENCE_ENGINE` при каждом запуске, поэтому держите `.env` равным
тому, что реально работает: обычный `make helm-up` с устаревшим значением
перенаправит имя модели. Разовый `make engine-up INFERENCE_ENGINE=vllm`
действует только на этот запуск. `make stack-up` поднимает движок из `.env`
тем же способом.

Закоммиченные значения (`inference.sglang.enabled`, `inference.ninfer.live`,
`router.modelServers`) - это значения рендера по умолчанию для vLLM; какой
движок работает на площадке, они больше не фиксируют.

## Изменение настроек движка

Флаги запуска - это values, а не манифесты: `helm/vllm-inference/values.yaml`
(`maxModelLen`, `maxNumSeqs`, `gpuMemoryUtilization`, ...) и
`helm/sglang-inference/values.yaml` (`contextLength`, `maxRunningRequests`,
`memFractionStatic`, ...). Применяйте `make vllm-up` / `make sglang-up`, пока
этот движок живой; перезапускается только его под. Что делает каждое
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

В `:9090/metrics` EPP `llm_d_epp_ready_endpoints` равен 1 для vLLM или
SGLang (0 при ninfer, который EPP не обслуживает), а
`llm_d_epp_flow_control_stale_endpoints` остаётся 0.

## Где это лежит

- Чарты: `helm/vllm-inference`, `helm/sglang-inference`,
  `helm/ninfer-inference`, `helm/embeddings-inference`
- Логика переключения: `Makefile` (`INFERENCE_ENGINE`, `engine-up`,
  `LLMD_ENGINE_SETS`, `HELM_ENGINE_SETS`)
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
