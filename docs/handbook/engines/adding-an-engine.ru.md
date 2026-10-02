# Подключение движка

[English](adding-an-engine.md) | [Оглавление справочника](../README.ru.md)

Три способа подключить движок помимо vLLM и SGLang, от наименее к наиболее
интегрированному, с ninfer и llama.cpp как разобранными примерами. Сначала
прочитайте [движки](README.ru.md) - как подключены движки первого класса.

**Содержание**

- [Выбор схемы](#выбор-схемы)
- [Схема A: отдельное имя модели (llama.cpp, внешний API)](#схема-a-отдельное-имя-модели-llamacpp-внешний-api)
- [Схема B: внутрикластерный движок под своим именем (ninfer)](#схема-b-внутрикластерный-движок-под-своим-именем-ninfer)
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
| B. внутрикластерный движок, своё имя | своё (`qwen-3.8-27b-ninfer`) | нет | OpenAI API | ninfer |
| C. за EPP, член пула | `qwen-3.8-27b` | да | OpenAI API, Prometheus `/metrics` в понятном EPP виде | vLLM, SGLang |

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

## Схема B: внутрикластерный движок под своим именем (ninfer)

Схема A для движка, который работает как GPU-Deployment в кластере: чарт по
образцу `helm/ninfer-inference` (запрос GPU, `nodeSelector` и toleration для
GPU-нод, без метки `llm-d.ai/model`), цель `<name>-up` в Makefile с
`--set replicas=$(<NAME>_REPLICAS)` и ветка в `engines-up`, а в
`helm/airgap-stack` пара `Backend`/`AIServiceBackend` и одно правило точного
совпадения на своё имя модели. Для ninfer это `inference.ninfer.enabled` и
`inference.ninfer.modelName: qwen-3.8-27b-ninfer`, правило `openai-ninfer` в
`llmd.yaml` ([ADR 0019](../../adr/0019-inference-plane-gpu-worker-nodes.md)).
Он не забирает `qwen-3.8-27b`: это выключило бы fair share для всех клиентов
пула.

Цена для его собственных клиентов: ни полос, ни справедливости между
пользователями, плюс все пробелы API движка (ninfer отказывает в JSON-режиме
`response_format`). При одной GPU он отвечает, только пока у пула 0 реплик.

## Схема C: первый класс за EPP

Для движка, который EPP понимает, поды нового движка входят в пул
`qwen-3.8-27b`:

1. Чарт движка по образцу `helm/sglang-inference`: `Recreate`,
   `nvidia.com/gpu: 1`, `nodeSelector` и toleration GPU-нод, порт 8000, без
   ключа API, метки пода `llm-d.ai/model: <имя модели>` и
   `llm-d.ai/engine-type: <type>`.
2. Makefile: `<NAME>_REPLICAS`, `ENGINE_DEPLOYMENT_<name>`, цель `<name>-up`
   с `--set replicas=$(<NAME>_REPLICAS)` и её ветки в `engines-up`.
3. Job Prometheus (обнаружение подов, порт 8000) и дашборды (ниже).
4. Проверить с одной репликой нового движка и нулём у остальных, затем
   смешанно, если хватает GPU: `llm_d_epp_ready_endpoints` считает всех
   членов пула.

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
  `qwen3_8_27b_nvfp4.ninfer` из MinIO через кэш моделей на ноде
  (собственный сконвертированный формат, не чекпойнт vLLM).
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
- [ ] Маршрут: values и правило схем A и B или метки пула схемы C
- [ ] Ключ апстрима в `.env` и `BackendSecurityPolicy`, если нужно
- [ ] `model_ids` Open WebUI обновлены, когда появляется новое имя модели
- [ ] Job Prometheus и хотя бы панель `up`
- [ ] `make verify`, smoke движка, `make llmd-nvfp4-smoke`
- [ ] Эта страница и [движки](README.ru.md) обновлены на обоих языках

## Связанные страницы

- Назад: [Движки](README.ru.md). Дальше: [Наблюдаемость](../observability/README.ru.md)
- Runbook: [inference-backends.md](../../operations/inference-backends.md)
