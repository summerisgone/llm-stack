# Подключение движка

[English](adding-an-engine.md) | [Оглавление справочника](../README.ru.md)

Три способа подключить движок помимо vLLM и SGLang, от наименее к наиболее
интегрированному, с ninfer и llama.cpp как разобранными примерами. Сначала
прочитайте [движки](README.ru.md) - как подключены движки первого класса.

**Содержание**

- [Выбор схемы](#выбор-схемы)
- [Схема A: отдельное имя модели (llama.cpp, внешний API)](#схема-a-отдельное-имя-модели-llamacpp-внешний-api)
- [Схема B: живой движок под каноническим именем (ninfer)](#схема-b-живой-движок-под-каноническим-именем-ninfer)
- [Схема C: первый класс за EPP](#схема-c-первый-класс-за-epp)
- [Контракт метрик](#контракт-метрик)
- [ninfer](#ninfer)
- [llama.cpp](#llamacpp)
- [Чеклист](#чеклист)
- [Связанные страницы](#связанные-страницы)

## Выбор схемы

| Схема | Имя модели | Очередь и fair share | Что нужно от движка | Пример |
| --- | --- | --- | --- | --- |
| A. отдельное имя модели | своё (`llamacpp-local`) | нет | OpenAI API | llama.cpp, внешний API |
| B. живой движок, прямой маршрут | `qwen-3.8-27b` | нет | OpenAI API, та же модель | ninfer |
| C. за EPP | `qwen-3.8-27b` | да | OpenAI API, Prometheus `/metrics` в понятном EPP виде | vLLM, SGLang |

Решающий вопрос для C: знает ли движок `core-metrics-extractor` llm-d
(встроены `vllm`, `sglang`, `trtllm-serve`, `triton-tensorrt-llm`, `triton`,
`atom`) или его можно научить именам метрик через `engineConfigs`. Если нет,
EPP не прочитает глубину очереди и занятость KV, пометит эндпоинт stale и
закроется.

## Схема A: отдельное имя модели (llama.cpp, внешний API)

Шлюз получает пару `Backend` + `AIServiceBackend` и одно правило точного
совпадения по новому имени модели
([ADR 0006](../../adr/0006-pluggable-inference-backends.md)). Шаблоны уже
есть в `helm/airgap-stack/templates/inference-backends.yaml` и `llmd.yaml`,
включаются через values:

```yaml
# helm/airgap-stack/values.yaml
inference:
  llamacpp:
    enabled: true            # маршрут появляется; ничего не запускается
    host: host.k3d.internal  # контейнер на хосте или DNS-имя Service
    port: 8090
    modelName: llamacpp-local
```

Затем `make helm-up`, добавьте id модели в `model_ids` подключения `0` Open
WebUI, если её должны видеть люди ([Open WebUI](../openwebui.ru.md#подключения-к-моделям)),
и положите ключ апстрима, если движку он нужен, в `.env`
(`EXTERNAL_API_KEY`). Действуют та же проверка JWT и тот же rate limit на
пользователя; очереди перед движком нет. Для нового типа движка скопируйте
блоки llamacpp под новым ключом `inference.<name>`.

## Схема B: живой движок под каноническим именем (ninfer)

Движок забирает `qwen-3.8-27b`, перенаправляя существующее правило
(`openai-qwen38nvfp4` и catch-all `openai` в `llmd.yaml`) на свой backend.
Клиенты изменений не видят; EPP пропускается. Для ninfer это
`inference.ninfer.enabled` (объекты существуют) плюс `inference.ninfer.live`
(маршрут указывает на него), а `make engine-up` выставляет `live` из
`INFERENCE_ENGINE=ninfer`. Второму движку такого типа понадобились бы свой
флаг `live` в тех же двух правилах и своя ветка в `ENGINE_DEPLOYMENT_*` и
`HELM_ENGINE_SETS` в Makefile.

Цена: ни полос, ни справедливости между пользователями, плюс все пробелы API
движка (ninfer отказывает в JSON-режиме `response_format`).

## Схема C: первый класс за EPP

Для движка, который EPP понимает:

1. Чарт движка по образцу `helm/sglang-inference`: одна реплика, `Recreate`,
   `nvidia.com/gpu: 1`, общий PVC с моделью, метки пода
   `app.kubernetes.io/name: <deployment>` и `llm-d.ai/engine-type: <type>`.
2. Makefile: `ENGINE_DEPLOYMENT_<name>`, `EPP_PORT_<name>`, добавить в
   `ENGINES` и в `EPP_ENGINE`, цель `<name>-up`.
3. Если нужен ключ апстрима - Secret и `BackendSecurityPolicy` на
   `llmd-qwen-test-openai`, как `sglang-api-key` в `llmd.yaml`.
4. Job Prometheus и дашборды (ниже).
5. Проверить переключение в обе стороны через `make engine-up`.

## Контракт метрик

[ADR 0015](../../adr/0015-third-party-engine-metrics-contract.md) делит то,
что должен дать движок, на три уровня:

| Уровень | Требование | Что даёт |
| --- | --- | --- |
| 1, обязателен | OpenAI `usage` в ответах (`prompt_tokens`, `completion_tokens`; `cached_tokens` по желанию) | учёт токенов и стоимости в pat-service, использование на дашборде |
| 2, обязателен | цель для Prometheus, хотя бы `up` | "жив ли" в Grafana |
| 3, по желанию | собственные метрики запросов движка (TTFT, очередь, KV) | дашборды движка, по одной панели за раз |

Job-ы сбора лежат в
`k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`
(статические цели с `namespace=airgap-ai-stack`); дашборды - в
`config/grafana/dashboards/`, загружаются `make monitoring-up`
([наблюдаемость](../observability/README.ru.md)).

## ninfer

- Чарт `helm/ninfer-inference` (движок плюс sidecar `jsonl_exporter.py`),
  образ `NINFER_IMAGE`, собранный из `NINFER_UPSTREAM_REF`, файл модели
  `ninfer-model-volume.yaml` (собственный сконвертированный формат, не
  чекпойнт vLLM).
- API-ключ: `NINFER_API_KEY` в `.env` -> Secret `ninfer-api-key` (создаётся
  `make helm-up`), его используют и сервер, и шлюз.
- Метрики: своих `/metrics` нет; sidecar превращает лог запросов в серии
  `ninfer_*` на `:9400` (job Prometheus `ninfer`, дашборд `ninfer.json`).
  Уровень 3 частичный: нет задержки на токен, KV в виде сырых страниц.
- Сильные стороны, измеренные в пилоте: MTP speculative decoding с
  маленькой draft-головой и префиксный кэш, который держался при длинных
  параллельных сессиях агентов. Подробности и риски:
  [deploy/ninfer](../../../deploy/ninfer/README.md).

## llama.cpp

Docker-контейнер на хосте (`deploy/llamacpp/run`, профиль
`config/llamacpp/qwen-gguf.env`), из кластера доступен по
`host.k3d.internal:8090`. Не развёрнут: нужен настоящий GGUF-файл с
проверенной контрольной суммой в `MODEL_GGUF`. Для учёта GPU в Kubernetes он
невидим, поэтому держите `N_GPU_LAYERS=0` (CPU), если GPU не свободен от
внутрикластерных движков. Метрики сейчас не собираются. Подробности:
[deploy/llamacpp](../../../deploy/llamacpp/README.md).

## Чеклист

- [ ] OpenAI-совместимые chat completions, стриминг и `usage` проверены напрямую на движке
- [ ] Чарт или запуск на хосте описан; образ закреплён в `versions.lock.env`
- [ ] Маршрут: values схемы A, флаг `live` схемы B или подключение к EPP схемы C
- [ ] Ключ апстрима в `.env` и `BackendSecurityPolicy`, если нужно
- [ ] `model_ids` Open WebUI обновлены, когда появляется новое имя модели
- [ ] Job Prometheus и хотя бы панель `up`
- [ ] `make verify`, smoke движка, `make llmd-nvfp4-smoke`
- [ ] Эта страница и [движки](README.ru.md) обновлены на обоих языках

## Связанные страницы

- Назад: [Движки](README.ru.md). Дальше: [Наблюдаемость](../observability/README.ru.md)
- Runbook: [inference-backends.md](../../operations/inference-backends.md)
