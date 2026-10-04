# Конфигурация

[English](README.md) | [Оглавление справочника](../README.ru.md)

Где лежит каждая настройка и как она попадает в кластер. Слоёв четыре:
`.env` (секреты и значения площадки, не коммитится), Helm values
(закоммиченное поведение), `versions.lock.env` (закреплённые версии) и
несколько блоков env сервисов в манифестах. Какая цель `make` применяет
каждый слой - часть ответа. Как превращать метрики в новые значения -
в [тюнинге](tuning.ru.md).

**Содержание**

- [Четыре слоя](#четыре-слоя)
- [Ключи .env](#ключи-env)
- [Как .env попадает в кластер](#как-env-попадает-в-кластер)
- [Helm values](#helm-values)
- [Настройки сервисов в манифестах](#настройки-сервисов-в-манифестах)
- [versions.lock.env](#versionslockenv)
- [Какая цель make что применяет](#какая-цель-make-что-применяет)
- [Значения площадки](#значения-площадки)
- [Где это лежит](#где-это-лежит)
- [Связанные страницы](#связанные-страницы)

## Четыре слоя

| Слой | Что содержит | В git | Правило |
| --- | --- | --- | --- |
| `.env` | учётные данные, публичный origin, флаги функций, живой движок | нет (`.env.example` - шаблон) | один ключ на настройку; секреты больше нигде |
| Helm values (`helm/*/values.yaml`, `config/*/values.yaml`) | поведение: лимиты, маршруты, флаги движков, tool server-ы | да | меняйте здесь, применяйте целью чарта |
| `versions.lock.env` | все версии и digest-ы образов и чартов | да | никогда не вписывайте версию в рецепт или манифест |
| Env в манифестах (`k8s/base/*.yaml`) | параметры сервисов без ключа в values (`QOS_*` pat-service, Open WebUI, брокер) | да | правьте base, `make helm-up` рендерит его в чарт |

## Ключи .env

Из `.env.example` (на настоящей площадке нужно задать каждый ключ оттуда)
плюс необязательные ключи, которые читает Makefile.

| Группа | Ключи | Кто использует |
| --- | --- | --- |
| Bootstrap и клиент Langfuse | `LANGFUSE_PUBLIC_KEY`, `LANGFUSE_SECRET_KEY`, `LANGFUSE_HOST`, `LANGFUSE_INIT_*` | первый старт Langfuse, экспорт из OTEL Collector |
| pat-service | `PAT_HASH_KEY`, `PAT_COOKIE_KEY` (каждый от 32 байт; смена ключа хэша убивает все PAT), `PAT_GATEWAY_CLIENT_SECRET`, `PAT_DIRECTORY_CLIENT_SECRET` (пустой отключает админку) | pat-service, клиенты Keycloak `pat-gateway` и `pat-directory` |
| Open WebUI | `WEBUI_SECRET_KEY` (держать стабильным), `OPENWEBUI_AUTOMATIONS_PAT` | сессии Open WebUI, подключение Automations |
| Движки | `VLLM_REPLICAS`, `SGLANG_REPLICAS`, `NINFER_REPLICAS`, `NINFER_API_KEY`, `EXTERNAL_API_KEY` | `make engines-up`, `<engine>-up`, `helm-up` |
| Флаги функций | `WEB_SEARCH_ENABLED`, `REPOWISE_ENABLED` | pat-service, инструменты Open WebUI, агенты, `stack-up` ([MCP](../mcp/README.ru.md)) |
| Площадка | `STACK_BASE_URL` (обязателен на удалённом профиле), `GRAFANA_BASE_URL`, `REPOWISE_PUBLIC_ORIGIN` | подстановка origin в Makefile, OIDC |
| repowise | `REPOWISE_API_KEY`, `REPOWISE_OIDC_CLIENT_SECRET`, `REPOWISE_PAT` | `make repowise-up` |
| Агенты (необязательно) | `PI_IMAGE`, `OPENCODE_IMAGE` переопределяют закреплённые; пустое значение исключает рантайм | `make agents-up` |
| Доступ оператора (необязательно) | `WSL_SSH_HOST`, `WSL_SSH_PORT` | `make k3s-tunnel`, загрузка образов |

## Как .env попадает в кластер

```
.env --(Makefile -include)--> переменные make: STACK_BASE_URL, *_REPLICAS, *_ENABLED, ...
  |
  +--(scripts/helm-env-values)--> helm/airgap-stack/runtime.secret.yaml (в .gitignore)
          `runtimeEnvironment:` каждая пара KEY=VALUE
       --(make helm-up)--> Secret airgap-runtime  --envFrom/secretKeyRef--> нагрузки
                      \--> производные Secret-ы: ninfer-api-key, external-api-key
                      \--> ConfigMap openwebui-tool-servers (записи, чей флаг "true")
  +--(make agents-up)--> ConfigMap agents/agent-images (образы и MCP-флаги)
  +--(make repowise-up)--> Secret-ы repowise, repowise-credential, repowise-oidc
```

- Каждый ключ `.env` попадает в Secret `airgap-runtime`, читает его
  какая-то нагрузка или нет.
- На профиле local-mac `make up` создаёт `airgap-runtime` прямо из `.env`.
- Изменение значения в Secret не перезапускает поды. После `make helm-up`
  перезапустите потребителей (`kubectl -n airgap-ai-stack rollout restart deploy/<name>`),
  если чарт не перекатил их сам.

## Helm values

| Файл | Ключевые настройки | Чем применять |
| --- | --- | --- |
| `helm/airgap-stack/values.yaml` | `inference.modelName`, `inference.perUserRateLimitPerMinute` (60), `inference.<backend>.enabled`, `inference.embeddings.*` (120/мин), `routing.*` (порт edge, пути, NodePort-ы), `oidc.*`, `openwebuiToolServers` | `make helm-up` |
| `helm/vllm-inference/values.yaml` | `inference.maxModelLen`, `maxNumSeqs`, `gpuMemoryUtilization`, `kvCacheDtype`, `prefixCaching`, `kvOffloadingSizeGiB`, ресурсы | `make vllm-up` |
| `helm/sglang-inference/values.yaml` | `inference.contextLength`, `maxRunningRequests`, `memFractionStatic`, настройки Mamba | `make sglang-up` |
| `helm/ninfer-inference/values.yaml` | параметры запуска ninfer | `make ninfer-up` |
| `helm/embeddings-inference/values.yaml` | `accelerator` (`cpu` или `gpu`), `gpu.gpuMemoryBudgetGiB`, лимиты батчей | `make embeddings-up` |
| `config/llmd/router-nvfp4-values.yaml` | полосы EPP, flow control, плагины, веса скореров | `make llmd-up` |
| `helm/web-search/values.yaml`, `helm/repowise/values.yaml` | нагрузки MCP-серверов | `make websearch-up`, `make repowise-up` |
| `config/gateway-addons/values.yaml` | Grafana (OIDC, datasource, дашборды, алерт), Loki, Tempo | `make monitoring-up` |
| `config/ai-gateway/values.yaml` | отображение заголовков в GenAI-спаны | `make gateway-up` |
| `config/gateway/remote-wsl-values.yaml` | контроллер Envoy Gateway | `make gateway-up` |

Значения, которые Makefile ставит поверх файлов: публичный origin
(`oidc.publicBaseURL`, `oidc.externalIssuer` из `STACK_BASE_URL`) и
`replicas` каждого релиза движка (из `VLLM_REPLICAS`, `SGLANG_REPLICAS`,
`NINFER_REPLICAS`).

## Настройки сервисов в манифестах

Хранятся как env в `k8s/base/applications.yaml` и рендерятся в чарт:

| Сервис | Настройки | Подробнее |
| --- | --- | --- |
| pat-service | коэффициенты полос и стоимости `QOS_*`, `MCP_CALLS_PER_MINUTE`, `AGENT_PAT_TTL_DAYS`, ConfigMap цен `pat-service-pricing` | [PAT-сервис](../pat-service.ru.md), [тюнинг](tuning.ru.md#коэффициенты-qos-в-pat-service) |
| Open WebUI | OIDC, роли, подключения, разрешения функций | [Open WebUI](../openwebui.ru.md) |
| agent-broker | `AGENT_SLOTS`, `IDLE_TIMEOUT`, runtime class, issuer (`k8s/agents/broker.yaml`) | [агенты](../agents/README.ru.md) |

Значения по умолчанию для настроек pat-service, которых нет в манифесте,
берутся из `loadConfig` в `pat-service/cmd/pat-service/main.go`.

## versions.lock.env

Makefile его `include`-ит. Группы: основные образы (Envoy, Keycloak, Open
WebUI, Postgres, ClickHouse, Valkey, MinIO, Langfuse, OTEL, Prometheus,
Grafana); vLLM и llm-d (версия чарта и digest-ы, образ EPP и digest);
дополнительные движки (SGLang, llama.cpp, ninfer); эмбеддинги; pat-service;
версии Helm-чартов операторов и шлюзов; агенты (`HERMES_IMAGE`,
`AGENT_CATALOG_IMAGE`, `AGENT_BROKER_IMAGE`, `PI_IMAGE`, `OPENCODE_IMAGE`);
веб-поиск; repowise. Обновление - это правка здесь плюс цель, которая
выкатывает компонент.

## Какая цель make что применяет

| Цель | Применяет |
| --- | --- |
| `make stack-up` | весь удалённый профиль по порядку (шлюзы, операторы, GPU-объекты, `helm-up`, `engines-up`, веб-поиск, если включён, провижининг) |
| `make helm-up` | `airgap-stack`: маршруты, rate limit-ы, отрендеренные нагрузки, `airgap-runtime`; затем провижининг клиентов Keycloak |
| `make engines-up` | применить реплики всех движков в безопасном для GPU порядке ([движки](../engines/README.ru.md#реплики-движков)) |
| `make vllm-up`, `sglang-up`, `ninfer-up`, `embeddings-up` | один релиз движка |
| `make llmd-up` | релиз EPP |
| `make monitoring-up` | Grafana, дашборды, Loki, Tempo |
| `make gateway-up` | Envoy Gateway, AI Gateway, затем `monitoring-up` |
| `make operators-up` | операторы CNPG, ClickHouse, Redis, MinIO |
| `make agents-up` | agent-broker и `agent-images` |
| `make websearch-up`, `make repowise-up` | MCP-серверы (перезапускают Open WebUI) |
| `make verify` | рендер и проверка всего, включая документацию, без кластера |
| `make helm-diff` | `helm-up` в режиме dry run |

## Значения площадки

Для новой площадки в [docs/install](../../install/README.md#values-that-are-site-specific)
перечислено всё, что нужно пересмотреть (origin, имя узла, каталог моделей,
NodePort-ы, realm). Значения площадки кладутся в `.env` или оверлеи
площадки, но никогда не литеральными хостами в отслеживаемые файлы;
`scripts/docs-check` падает, если находит в документации origin из `.env`.

## Где это лежит

- `.env.example`, `scripts/helm-env-values`, `Makefile`, `versions.lock.env`
- Инвентарь учётных данных и ротация: [docs/security](../../security/README.md#credential-inventory)

## Связанные страницы

- Назад: [Данные и сроки хранения](../observability/data-and-retention.ru.md). Дальше: [Тюнинг](tuning.ru.md)
