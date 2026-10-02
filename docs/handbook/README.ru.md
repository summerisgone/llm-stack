# Справочник llm-stack

[English](README.md)

## Содержание

| # | Страница | О чём |
| --- | --- | --- |
| 1 | [Обзор системы](overview.ru.md) | компоненты, namespace-ы, три пути запроса, идентичность, владение объектами в репозитории |
| 2 | [Open WebUI](openwebui.ru.md) | что получают пользователи, подключения, вход и роли, чего ожидать |
| 3 | [PAT-сервис](pat-service.ru.md) | токены, склейка сессий, выбор полосы, учёт, чего сервис не делает |
| 4 | [Инференс](inference/README.ru.md) | путь от шлюза до GPU, кто что решает, обходные пути, таймауты |
| 4.1 | [Очереди и fair share](inference/queues-and-fair-share.ru.md) | полосы EPP, справедливость между пользователями, насыщение, градуированный потолок |
| 4.2 | [KV-кэш](inference/kv-cache.ru.md) | память GPU, переиспользование префиксов, настройки движков, контекст против параллелизма |
| 5 | [Движки](engines/README.ru.md) | пул `qwen-3.8-27b`, реплики движков, `make engines-up`, эмбеддинги |
| 5.1 | [Подключение движка](engines/adding-an-engine.ru.md) | ninfer, llama.cpp, внешние API, контракт метрик |
| 6 | [Наблюдаемость](observability/README.ru.md) | источники метрик, дашборды по вопросам, логи, алерты |
| 6.1 | [Трейсы Langfuse](observability/langfuse.ru.md) | что в трейсе, пользователи и сессии |
| 6.2 | [Данные и сроки хранения](observability/data-and-retention.ru.md) | что где хранится, сроки хранения, удаление данных пользователя |
| 7 | [Конфигурация](configuration/README.ru.md) | `.env`, Helm values, версии, какая цель что применяет |
| 7.1 | [Тюнинг лимитов и очередей](configuration/tuning.ru.md) | rate limit-ы, EPP, коэффициенты pat-service, ёмкость движка - по метрикам |
| 8 | [Агенты](agents/README.ru.md) | agent-broker, рантаймы, профили, токены, gVisor |
| 8.1 | [Управление агентами](agents/managing.ru.md) | деплой, каталог, ключи, offboarding, новый рантайм |
| 9 | [MCP-серверы](mcp/README.ru.md) | установленные серверы, дверь через pat-service, Open WebUI и агенты, политики |
| 9.1 | [Repowise](mcp/repowise.ru.md) | эксплуатация repowise |
| 9.2 | [Подключение MCP-сервера](mcp/adding-a-server.ru.md) | шаги и порядок выката |
| 10 | [Эксплуатация](operations.ru.md) | доступ к кластеру, деплой, smoke-тесты, пользователи, известные сбои и пробелы |
| 11 | [Как дописывать документацию](contributing.ru.md) | куда писать изменение, шаблон страницы, английский и русский, docs-check |

## Глоссарий

| Термин | Значение | Где определён |
| --- | --- | --- |
| PAT | персональный токен доступа `sk-...`, учётные данные для `/v1` | [PAT-сервис](pat-service.ru.md#жизненный-цикл-токена) |
| ключ агента | PAT на 7 дней с `issued_by = agents`, которым пользуются агенты пользователя | [Агенты](agents/README.ru.md#токены) |
| EPP | llm-d endpoint picker: очередь и планировщик перед движком | [Очереди и fair share](inference/queues-and-fair-share.ru.md) |
| полоса (band) | класс приоритета EPP: `warm` 10, `normal` 5, `demoted` 1, запасная 0 | [Очереди и fair share](inference/queues-and-fair-share.ru.md#полос-четыре-а-не-одна) |
| fairness id | ключ, по которому EPP делит полосу: Keycloak `sub` пользователя | [Очереди и fair share](inference/queues-and-fair-share.ru.md#справедливость-внутри-полосы) |
| ключ сессии | chain-hash идентификатор разговора в pat-service, `sess-...` | [PAT-сервис](pat-service.ru.md#склейка-сессий) |
| насыщение (saturation) | сигнал нагрузки EPP от 0 до 1 по очереди движка и занятости KV | [Очереди и fair share](inference/queues-and-fair-share.ru.md#насыщение-когда-epp-придерживает-запросы) |
| реплики движков | сколько подов каждого движка работает (`VLLM_REPLICAS`, `SGLANG_REPLICAS`, `NINFER_REPLICAS`); поды vLLM и SGLang обслуживают `qwen-3.8-27b` | [Движки](engines/README.ru.md#реплики-движков) |
| рантайм | вид агента: Hermes, pi или opencode | [Агенты](agents/README.ru.md#рантаймы) |
| слот | одно из K мест для запущенного пода агента | [Агенты](agents/README.ru.md#agent-broker) |
| профиль | домашний каталог агента пользователя на PVC | [Агенты](agents/README.ru.md#профили-и-каталог) |
| каталог | курируемые навыки и настройки, которые раскладываются в каждый профиль | [Агенты](agents/README.ru.md#профили-и-каталог) |
| MCP-дверь | pat-service `/mcp/<name>/`, единственный путь к MCP-серверу | [MCP-серверы](mcp/README.ru.md#дверь-pat-service-mcp) |

## Другая документация

Эти документы на английском.

| Где | Что |
| --- | --- |
| [README.md](../../README.md) | стек на одной странице, чарты, профили, команды деплоя |
| [AGENTS.md](../../AGENTS.md) | правила репозитория для людей и coding-агентов |
| [docs/architecture](../architecture/README.md) | архитектурный справочник |
| [docs/adr](../adr/README.md) | решения и их история (после принятия не редактируются) |
| [docs/install](../install/README.md) | установка на новую площадку |
| [docs/operations](../operations/README.md) | runbook-и |
| [docs/security](../security/README.md) | границы доверия, инвентарь учётных данных |
| [docs/clients](../clients/README.md) | подключение клиентов (coding-инструменты, SDK) |
| [docs/airgap](../airgap/README.md) | статус air-gap сборки |
