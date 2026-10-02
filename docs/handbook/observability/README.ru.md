# Наблюдаемость

[English](README.md) | [Оглавление справочника](../README.ru.md)

Три сигнала - три места: **метрики** в Prometheus, показанные в Grafana;
**трейсы** каждого вызова модели (промпт, ответ, пользователь, сессия) в
Langfuse; **логи** контейнеров в Loki через Grafana. Здесь описано, кто что
отдаёт, как до этого добраться и какой дашборд отвечает на какой вопрос.
Модель трейсов - на странице [Langfuse](langfuse.ru.md); что хранится и
сколько - в [данных и сроках хранения](data-and-retention.ru.md).

**Содержание**

- [Карта](#карта)
- [Как попасть в интерфейсы](#как-попасть-в-интерфейсы)
- [Источники метрик](#источники-метрик)
- [Дашборды по вопросам](#дашборды-по-вопросам)
- [Логи](#логи)
- [Алерты](#алерты)
- [Пробелы](#пробелы)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Карта

```
движки, EPP, pat-service, узел, GPU --сбор 15s--> Prometheus (airgap-ai-stack)
                                                          |  datasource "vLLM Prometheus"
GenAI-спаны AI Gateway --OTLP--> OTEL Collector --> Langfuse (web + worker; Postgres, ClickHouse, MinIO, Valkey)
                                                          |
логи контейнеров --fluent-bit--> Loki (monitoring)        v
метрики Envoy --> Prometheus (monitoring, eg-addons)  Grafana (monitoring, eg-addons)
```

Экземпляров Prometheus два: прикладной в `airgap-ai-stack` (обычный
Deployment, все метрики LLM) и встроенный в add-ons Envoy Gateway в
`monitoring` (метрики Envoy). Операторская Grafana - та, что из add-ons в
`monitoring`; она читает оба. Второй, старый Deployment Grafana в
`airgap-ai-stack` использовать не нужно.

## Как попасть в интерфейсы

Публичного маршрута нет ни у Grafana, ни у Langfuse. Они выставлены как
NodePort-ы на GPU-хосте (`routing.nodePorts` в
`helm/airgap-stack/values.yaml`: Grafana 32030, Langfuse 32031) и доступны из
сети операторов. Адреса площадки - `GRAFANA_BASE_URL` и патч публичного URL
Langfuse (`k8s/overlays/remote-wsl-vllm-nvfp4/langfuse-public-url-patch.yaml`).

| Интерфейс | Вход | Для кого |
| --- | --- | --- |
| Grafana | только Keycloak SSO (форма входа выключена); `ai-admin` - Grafana Admin, остальные - Viewer | операторы, тимлиды |
| Langfuse | собственные учётные записи; первый администратор из `LANGFUSE_INIT_USER_*` в `.env` | только операторы: там видны промпты и ответы |
| Prometheus | UI не выставлен; `kubectl -n airgap-ai-stack port-forward svc/prometheus 9090` | разовые PromQL-запросы |

## Источники метрик

Job-ы сбора прикладного Prometheus
(`k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`), все с
`namespace=airgap-ai-stack`:

| Job | Цель | Префикс метрик | Что показывает |
| --- | --- | --- | --- |
| `vllm-qwen38-nvfp4` | `:8000/metrics` | `vllm:` | очередь движка, занятость KV, TTFT, пропускная способность, попадания в префиксный кэш |
| `sglang-qwen38` | `:8000/metrics` | `sglang:` | то же для SGLang |
| `ninfer` | sidecar `:9400` | `ninfer_` | переэкспортированный лог запросов ninfer |
| `embeddings-bge-m3` | `:80` | метрики text-embeddings-inference | сервер эмбеддингов |
| `llmd-epp` | EPP `:9090` | `llm_d_epp_` (и устаревший `inference_extension_`) | очереди по полосам, насыщение, TTFT по пользователям, готовые и устаревшие эндпоинты |
| `pat-service` | `:9090` | `patsvc_` | запросы по пользователям и полосам, токены, стоимость, сессии, вызовы MCP |
| `node` | node-exporter | `node_` | CPU, память, диск, сеть хоста |
| `gpu-exporter` (DaemonSet на GPU-нодах) | `:9400` | `gpu_` | загрузка, память, температура GPU (`nvidia-smi`; DCGM на WSL2 не работает) |

У метрик pat-service есть метка `user` (Keycloak `sub`); у метрик EPP -
`fairness_id` (тот же `sub` или `default-flow` для трафика Open WebUI).
Сопоставить `sub` с именем можно в Keycloak или в Langfuse.

Этот Prometheus не собирает: Envoy (дашборды envoy-gateway читают
собственный Prometheus чарта add-ons), agent-broker (метрик пока нет),
web-search-mcp, Langfuse, ClickHouse, kube-state-metrics и cAdvisor (панелей
ресурсов по подам нет).

## Дашборды по вопросам

Папки в Grafana, загружаются из `config/grafana/dashboards/` командой
`make monitoring-up`:

| Вопрос | Дашборд (папка / имя) |
| --- | --- |
| Всё ли живо? | cluster-monitor / Cluster monitor (`up{job}` для каждой цели, вид узла) |
| Занят ли GPU, горячий ли, полон ли? | system-state / System state |
| Какой лимит или параметр очереди менять? | QoS / Cluster load (сделан ровно для этого) |
| Кто сколько использует и с какой задержкой? | QoS / User activity (токены, TTFT, стоимость, исходы по пользователям, устаревшие эндпоинты) |
| Склеиваются ли сессии, какие полосы назначаются? | QoS / Fair share |
| Внутренности движка: running, waiting, KV, TTFT | vLLM / vLLM; llm-d / vLLM overview или SGLang overview |
| Очереди EPP, размеры запросов, задержка планирования | llm-d / Inference Gateway, Failure and saturation, Diagnostic drill-down, Performance and KV cache |
| ninfer | ninfer |
| Трафик шлюза, 429, задержка апстрима | envoy-gateway (из чарта add-ons) |

Дашборд llm-d для prefill/decode остаётся пустым: на этой площадке prefill и
decode не разделены.

## Логи

fluent-bit отправляет логи контейнеров в Loki в `monitoring`; смотрите их в
Grafana Explore с datasource Loki. Срок хранения - по умолчанию чарта
([данные и сроки хранения](data-and-retention.ru.md)). Для быстрой проверки
`kubectl logs` быстрее:

```sh
kubectl -n airgap-ai-stack logs deploy/pat-service --tail=100
kubectl -n airgap-ai-stack logs deploy/llmd-qwen-test-epp -c epp --tail=100
kubectl -n agents logs deploy/agent-broker --tail=100
```

## Алерты

Заведено три правила алертов Grafana (`config/gateway-addons/values.yaml`,
`grafana.alerting`):

| Правило | Срабатывает при | Значение |
| --- | --- | --- |
| `adr0012-stale-endpoints` (critical) | `max_over_time(llm_d_epp_flow_control_stale_endpoints[5m]) > 0` | EPP не может прочитать метрики движка и перестал в него отправлять |
| `adr0019-no-ready-endpoints` (critical) | `max_over_time(llm_d_epp_ready_endpoints[5m]) < 1` или нет данных | у пула `qwen-3.8-27b` 5 минут нет готового пода; ожидаемо, только когда его реплики намеренно 0 |
| `adr0019-metrics-errors` (warning) | ошибки опроса или разбора метрик в EPP росли 10 минут, держится 5 минут | `/metrics` члена пула недоступен или не разбирается (GPU-нода упала, нет `llm-d.ai/engine-type`, NetworkPolicy) |

Точки доставки (contact point) в репозитории нет; настройте её в Grafana,
если кого-то нужно будить.

## Пробелы

- Сроки хранения не заданы нигде: у Prometheus, Loki, Tempo, Langfuse и его
  хранилищ - значения по умолчанию ([данные и сроки хранения](data-and-retention.ru.md)).
- У agent-broker нет метрик; активность агентов видна только в метриках
  pat-service (как PAT агента пользователя) и в логах.
- Панели `patsvc_cost_units_total` строились, когда разбирался только
  нестриминговый usage; теперь pat-service читает и стриминговый, так что
  старые данные занижают трафик coding-агентов.
- SGLang 0.5.19 отдаёт `sglang:*`, а вендоренный дашборд llm-d / SGLang
  overview запрашивает `sglang_*`, поэтому его панели пусты; QoS / Fair
  share использует правильные имена.

## Где это лежит

- Prometheus: `k8s/base/observability.yaml`, конфигурация сбора
  `k8s/overlays/remote-wsl-vllm-nvfp4/prometheus-config-patch.yaml`
- Grafana, Loki, Tempo: `config/gateway-addons/values.yaml`, `make monitoring-up`
- Дашборды и их происхождение: `config/grafana/dashboards/`
  ([README](../../../config/grafana/dashboards/README.md))
- OTEL Collector и Langfuse: `k8s/base/observability.yaml`,
  `k8s/base/applications.yaml`
- Тест телеметрии: [tests/telemetry](../../../tests/telemetry/README.md)

## Связанные страницы

- Назад: [Подключение движка](../engines/adding-an-engine.ru.md). Дальше: [Langfuse](langfuse.ru.md)
- [Тюнинг по метрикам](../configuration/tuning.ru.md)
