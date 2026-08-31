# Руководство пользователя кластера LLM Stack

> **Для кого этот документ.** Администратор кластера или тимлид, который управляет пользователями, квотами и хочет понять, что показывает мониторинг — и что с этим делать.

---

## 1. Управление пользователями

### Как устроена идентификация

Все пользователи живут в Keycloak, realm **`ai-stack`**. Два способа обратиться к кластеру:

- **Браузер** → Open WebUI (`https://<origin>/`) — вход через Keycloak SSO, токен не нужен.
- **API / инструменты** → `https://<origin>/v1` — нужен персональный токен (PAT, формат `sk-…`), который выдаётся на дашборде `https://<origin>/platform`.

Запросы через оба канала несут одинаковый набор заголовков идентификации (`X-User-Id`, `X-User-Name`), поэтому в метриках и трейсах пользователь виден одинаково независимо от способа подключения.

### Роли

| Роль | Что даёт |
|---|---|
| `ai-user` | Доступ к Open WebUI и PAT дашборду, использование модели |
| `ai-admin` | То же + роль Admin в Grafana |
| `observability-viewer` | Зарезервировано под будущий read-only доступ к мониторингу |

Пользователь без роли `ai-user` или `ai-admin` получает отказ от Open WebUI при входе.

### Создание пользователя

Управление пользователями происходит через **Keycloak Admin Console** — она не выставлена наружу, только через port-forward или NodePort на хосте кластера.

```bash
# Открыть туннель к Keycloak Admin API
kubectl -n airgap-ai-stack port-forward svc/keycloak 8888:8080

# Теперь Admin Console доступна по http://localhost:8888/sso/admin
# Логин/пароль — из KC_BOOTSTRAP_ADMIN_* в k8s/base/applications.yaml
```

В Admin Console: **Manage → Users → Add user**. После создания:

1. Вкладка **Credentials** → задать пароль → отключить «Temporary» если не хотите, чтобы пользователь менял его при первом входе. Политика реалма: минимум 12 символов, обязательно строчная, прописная, цифра и спецсимвол.
2. Вкладка **Role mapping** → назначить `ai-user` или `ai-admin`.
3. При первом входе через браузер пользователю предложат настроить TOTP (обязательно — включена защита брутфорса).

### Создание PAT (API-токена)

Пользователь делает это самостоятельно через `https://<origin>/platform`. Токен показывается **один раз** — хранится только его HMAC-хэш. Несколько токенов у одного пользователя не увеличивают лимиты: rate limit считается по владельцу, а не по токену.

Для отзыва: в том же дашборде → кнопка Revoke. Эффект — на следующем запросе.

### Удаление пользователя

В Keycloak Admin Console → Users → Delete. Активные PAT-токены этого пользователя после этого возвращают 401 (сервис не найдёт владельца при валидации).

---

## 2. Лимиты в AI Gateway

### Что ограничивается и где

Лимиты применяются на двух уровнях — независимо и аддитивно:

| Уровень | Политика | Значение | По чему считается |
|---|---|---|---|
| Edge (внешний Envoy) | `edge-api-ip-rate-limit` | 60 req/min | IP-адрес источника |
| AI Gateway (внутренний) | `llmd-per-user-limit` | **60 req/min** | `X-User-Id` (владелец PAT или SSO subject) |
| SSO token endpoint | `edge-sso-token-rate-limit` | 10 req/min | IP-адрес |
| SSO общий prefix `/sso` | `edge-sso-rate-limit` | 100 req/min | IP-адрес |

Основной рабочий лимит для пользователей — **60 запросов в минуту** на пользователя на инференс-роуте — это защита от шторма запросов, а не контроль параллелизма: сколько слотов `--max-num-seqs` пользователь занимает одновременно, решает `pat-service` (`TASK-qos-fair-share.md`, `docs/adr/0008-per-user-fair-share.md`), не этот лимит. Раньше здесь стояло 3 req/min — временное значение на время замеров Этапа 0 задачи QoS; поднято при переходе к Этапу 3, чтобы не мешать сбору реального трафика. При превышении Envoy возвращает `429 Too Many Requests` с заголовками `X-RateLimit-*` (Draft Version 03).

### Как изменить per-user лимит

Лимит — `helm/airgap-stack/values.yaml` `inference.perUserRateLimitPerMinute`, подставляется в `BackendTrafficPolicy/llmd-per-user-limit` (`helm/airgap-stack/templates/llmd.yaml`). Менять через values, не правкой шаблона:

```yaml
inference:
  perUserRateLimitPerMinute: 60   # ← количество запросов в минуту
```

Изменить:

```bash
# Отредактировать значение в репозитории, затем применить Helm-релиз
make helm-up
```

`make helm-up` сам перерисовывает чарт из kustomize-исходников и подкладывает
`.env` поверх values — вызывать `helm upgrade` руками не нужно.

Изменение применяется без рестарта подов — Envoy подхватывает новую политику через xDS.

### Как изменить edge IP-лимит

Аналогично, в `routes-external.yaml`, политика `edge-api-ip-rate-limit`. Актуально если пользователи сидят за NAT и все приходят с одного IP.

---

## 3. Управление очередями в llm-d EPP

### Как работает EPP

llm-d Endpoint Picker (EPP) стоит между AI Gateway и движком инференса (vLLM или SGLang — живым всегда остаётся только один, GPU один). Он принимает запрос через ext-proc, оценивает состояние бэкенда и либо пропускает запрос немедленно, либо ставит в очередь.

Текущая конфигурация (`config/llmd/router-nvfp4-values.yaml`):

| Параметр | Значение | Смысл |
|---|---|---|
| `flowControl.maxRequests` | 32 | Максимум запросов одновременно в EPP (очередь + исполнение) |
| `flowControl.maxBytes` | 2 Gi | Лимит суммарного объёма тел запросов в очереди |
| `flowControl.defaultRequestTTL` | 10 min | Запрос, не дождавшийся слота за 10 минут, отбрасывается |
| `concurrency-detector.maxConcurrency` | 3 | Сколько запросов движок обрабатывает одновременно. Должно совпадать с лимитом живого движка: SGLang `--max-running-requests` (`inference.maxRunningRequests`), vLLM `--max-num-seqs` |
| `failOpen` | false | При недоступности EPP запросы не проходят (fail-closed) |

### Приоритетные полосы

Полос **четыре**, и по одному только `values.yaml` это не видно — там объявлена
одна. Полосы `10`/`5`/`1` EPP провижнит динамически из объектов
`InferenceObjective`, перечисленных в `router.inferenceObjectives`; полоса `0`
— статическая запись в `flowControl.priorityBands`. В логах EPP при старте
видно `"Provisioning priority band from control plane"` для 1, 5 и 10, а в его
`/metrics` появляются `inference_extension_flow_control_*` сразу для всех
четырёх приоритетов, ещё до первого запроса.

| Полоса | Priority | Кто сюда попадает |
|---|---:|---|
| `warm` | 10 | Прогретая сессия — `pat-service` ставит `x-llm-d-inference-objective: warm` |
| `normal` | 5 | Новая сессия, а также деградация при недоступном Valkey |
| `demoted` | 1 | Сессия отработала подряд `N` шагов либо пользователь ушёл вперёд по расходу GPU |
| *(fallback)* | 0 | Запрос **без** заголовка `x-llm-d-inference-objective` |

Полосу выбирает `pat-service` по состоянию сессии; произвольное значение
приоритета в заголовке передать нельзя — запрос может выбрать только среди
существующих объектов `InferenceObjective`.

**Важное следствие:** большая цифра обслуживается раньше, поэтому fallback-полоса
`0` стоит **ниже** `demoted`. Трафик, идущий мимо `pat-service` (сегодня это
Open WebUI), попадает в самый низ очереди. Заголовок objective ставит только
`pat-service`.

Явные `fairnessPolicyRef` (**round-robin**) и `orderingPolicyRef` (**FCFS**,
first come, first served) заданы только у полосы `0`; полосы 10/5/1 получают
политики по умолчанию.

### Скоринг запросов

EPP оценивает каждый запрос по трём критериям перед выбором бэкенда:

- **queue-scorer** (вес 2) — предпочитает бэкенд с меньшей очередью.
- **kv-cache-utilization-scorer** (вес 2) — предпочитает бэкенд с меньшей загрузкой KV-кэша.
- **prefix-cache-scorer** (вес 3, приоритетный) — предпочитает бэкенд, у которого уже закэширован prefix текущего промпта. Это ключевой выигрыш при повторяющихся системных промптах.

### Как изменить параметры очереди

Все параметры EPP — в `config/llmd/router-nvfp4-values.yaml`. Применяется через Helm-релиз `llmd`:

```bash
make llmd-up
```

**Если растёт очередь и запросы таймаутятся** — в первую очередь смотреть на `maxConcurrency`. Это значение должно совпадать с лимитом живого движка (SGLang `--max-running-requests`, vLLM `--max-num-seqs`). Если движок принимает больше — EPP будет недоиспользовать GPU. Если меньше — EPP будет отвергать запросы раньше, чем движок насытится.

При смене движка `maxConcurrency` двигается **вместе** с `router.modelServers`
и `core-metrics-extractor.defaultEngine` — см. `docs/operations/inference-backends.md`.

**Если запросы дропаются через 10 минут** — либо увеличить `defaultRequestTTL`, либо (правильнее) снизить per-user rate limit, чтобы меньше запросов попадало в очередь.

---

## 4. Интерпретация статистики

Статистика собирается в двух местах с разной детализацией.

### 4.1 Langfuse — трейсы запросов

URL: `http://langfuse.gpu-host.local:32031` (NodePort, только из сети оператора).

Langfuse показывает каждый вызов модели как **generation** с полями:

| Поле | Источник | Что смотреть |
|---|---|---|
| **User** | `X-User-Name` (PAT owner или SSO username) | Кто сделал запрос |
| **Input / Output** | Промпт и ответ модели | Содержание (для аудита, отладки) |
| **Model** | `llm.model_name` | Какая модель вызвана |
| **Token usage** | `llm.token_count.prompt` / `completion` / `total` | Нагрузка на GPU по пользователям |
| **Latency** | Длительность span | Сколько запрос занял |
| **Metadata** | `pat_token_id`, SSO subject | Какой PAT использован |

Токены, выданные до того, как PAT-сервис начал сохранять username, показывают SSO subject вместо имени. Выдайте пользователю новый PAT — следующие трейсы покажут имя.

**Ключевые паттерны:**
- Высокий `prompt_tokens` при коротком `completion_tokens` — длинный системный промпт, хорошо посмотреть на prefix-cache hit rate в Grafana.
- Много мелких запросов от одного пользователя с одинаковым prefix — хорошие кандидаты для выигрыша от prefix-cache.
- Растущее время трейса без роста токенов — признак очереди в EPP или конкуренции на GPU.

### 4.2 Grafana — метрики инфраструктуры

URL: `http://grafana.gpu-host.local:32030` (NodePort). Вход через Keycloak SSO (роль `ai-admin` даёт доступ Admin).

Три папки дашбордов:

#### Папка `vLLM`
Метрики самого движка инференса. Ключевые графики:

| График | Что читать |
|---|---|
| **Running / Waiting requests** | `running` — запросы в GPU прямо сейчас, `waiting` — в очереди vLLM. Если `waiting` > 0 стабильно — GPU насыщен. |
| **GPU KV Cache Usage %** | При >80% вырастает latency из-за вытеснения токенов. |
| **Time to First Token (TTFT)** | Время до первого токена ответа. Растёт при длинных промптах или высоком `waiting`. |
| **Token Throughput (generation)** | Токенов/сек на выходе. Практический потолок GPU на текущей нагрузке. |
| **Prefix Cache Hit Rate** | Доля запросов с попаданием в prefix-кэш. При высоком значении — эффективный reuse промптов. |

#### Папка `llm-d`
Метрики EPP. Ключевые:

| График | Что читать |
|---|---|
| **Queue depth** | Сколько запросов ждут слота в EPP. Стабильно > 0 при текущем лимите — сигнал к снижению лимита или росту `maxConcurrency`. |
| **Request TTL expirations** | Дропы по таймауту очереди. Если ненулевое — пользователи получают ошибки, а не ответы. |
| **Concurrency** | Реальная нагрузка vs `maxConcurrency`. |

#### Папка `envoy-gateway`
Метрики gateway. Ключевые:

| График | Что читать |
|---|---|
| **Rate limit hits** | Количество `429` по политикам. Рост — признак того, что лимит слишком низкий или атака. |
| **Request rate by route** | Трафик на `/v1` vs `/sso` vs WebUI. |
| **Upstream response time** | Latency между gateway и pat-service/EPP. |

---

## 5. Когда и как редактировать лимиты по наблюдениям

### Сигналы для снижения лимита (пользователи делают слишком много)

- `vLLM → Waiting requests` стабильно > 0 при нескольких активных пользователях.
- `llm-d → Queue depth` ненулевой в рабочее время.
- `llm-d → TTL expirations` > 0 — кто-то уже получает ошибки.
- `TTFT` вырос и не связан с длиной промптов.

**Действие:** снизить `limit.requests` в `llmd-per-user-limit` или добавить дифференцированные политики по ролям (например, `ai-admin` — 10 req/min, `ai-user` — 3 req/min).

### Сигналы для повышения лимита (GPU простаивает)

- `vLLM → Running requests` стабильно < `maxConcurrency` (= `--max-num-seqs` у vLLM, `--max-running-requests` у SGLang).
- `vLLM → GPU KV Cache Usage` < 40% в рабочие часы.
- `Token Throughput` значительно ниже измеренного пика (см. `docs/operations/vllm-inference.md`).
- В Langfuse видны пользователи с длинными сессиями, но редкими запросами.

**Действие:** повысить `limit.requests` или `limit.unit` (например, с Minute на Hour с пропорционально большим числом).

### Сигналы для изменения параметров EPP

| Наблюдение | Что менять |
|---|---|
| `TTL expirations` растут, `Waiting` умеренный | Увеличить `defaultRequestTTL` (пользователи готовы ждать) или снизить per-user rate limit (меньше запросов попадает в очередь) |
| `Waiting` = 0, GPU недогружен | Увеличить `maxConcurrency` (синхронно с лимитом живого движка) |
| Запросы с одним prefix медленные | Проверить `Prefix Cache Hit Rate` — если низкий при повторяющихся промптах, проверить что `prefix-cache-scorer` активен в EPP конфиге |
| Один пользователь «занимает» всю очередь | Полосы уже есть (10/5/1 + fallback 0), но `flowControl` не ограничивает параллелизм **на пользователя** — round-robin работает только при выборке из очереди. Проверить, что `pat-service` действительно проставляет objective, и смотреть в сторону per-band `maxRequests` либо потолка слотов на пользователя |

### Workflow изменения лимита

```
1. Зафиксировать текущее состояние:
   - скриншот / экспорт из Grafana (vLLM waiting, TTFT, throughput)
   - период наблюдения минимум 30 минут в рабочее время

2. Изменить одну переменную за раз:
   - отредактировать yaml в репозитории
   - helm upgrade (применяется горячо, без рестарта)

3. Наблюдать не менее 15 минут после применения:
   - если TTFT не вырос и waiting = 0 — лимит принят
   - если TTL expirations появились — откатить или снизить шаг

4. Зафиксировать в commit message: старое значение, новое, причину.
```

---

## 6. Выбор движка инференса

В кластере два движка на одном и том же чекпоинте `RadixArk-Qwen3.8-27B-NVFP4`,
каждый — отдельный Helm-релиз:

| Движок | Релиз | Команда |
|---|---|---|
| vLLM (по умолчанию) | `helm/vllm-inference` | `make vllm-up` / `make vllm-down` |
| SGLang | `helm/sglang-inference` | `make sglang-up` / `make sglang-down` |

Имя модели в API — одно и то же для обоих движков: **`qwen-3.8-27b`**.
Клиенты всегда отправляют его в поле `model`, независимо от того, какой
движок сейчас обслуживает запрос; переключение движка не меняет имя модели.

**GPU один.** Оба Deployment запрашивают `nvidia.com/gpu: 1`, поэтому второй
под просто зависает в `Pending` — планировщик не даст им подраться за карту.
Переключение:

```bash
kubectl -n airgap-ai-stack scale deployment/vllm-qwen38-nvfp4 --replicas=0
make sglang-up
```

Обратно — наоборот. `make stack-up` поднимает только vLLM.

Модель видна в выпадающем списке Open WebUI и в `/v1/models` как
`qwen-3.8-27b` **независимо от того, что реально запущено**: список маршрутов
(`inference.sglang.enabled` в `helm/airgap-stack/values.yaml`) определяет,
какому движку правило `qwen-3.8-27b` отдаёт запрос, а список запущенных
релизов — что реально отвечает. Запрос к остановленному движку вернёт
ошибку. Предупредите пользователей перед переключением.

Параметры запуска — values, а не манифесты:
`helm/vllm-inference/values.yaml` (`maxModelLen`, `maxNumSeqs`,
`gpuMemoryUtilization`, `kvCacheDtype`, ...) и
`helm/sglang-inference/values.yaml` (`contextLength`, `maxRunningRequests`,
`memFractionStatic`, ...). Правка одного флага перезапускает только под
движка, весь стек не трогает. Подробности —
`docs/operations/inference-backends.md`.

---

## Быстрая справка

```
# Посмотреть текущие rate limit политики
kubectl -n airgap-ai-stack get backendtrafficpolicies

# Посмотреть очередь EPP (логи)
kubectl -n airgap-ai-stack logs -l app.kubernetes.io/component=epp --tail=50

# Посмотреть статус подов
kubectl -n airgap-ai-stack get pods

# Какой движок сейчас держит GPU
kubectl -n airgap-ai-stack get deploy vllm-qwen38-nvfp4 sglang-qwen38

# Grafana (оператор)
http://grafana.gpu-host.local:32030

# Langfuse (оператор)
http://langfuse.gpu-host.local:32031

# Keycloak Admin (через туннель)
kubectl -n airgap-ai-stack port-forward svc/keycloak 8888:8080
# → http://localhost:8888/sso/admin
```
