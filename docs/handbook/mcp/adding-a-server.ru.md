# Подключение MCP-сервера

[English](adding-a-server.md) | [Оглавление справочника](../README.ru.md)

Шаги, чтобы поставить новый MCP-сервер за pat-service и сделать его
доступным Open WebUI и всем рантаймам агентов. Схема та же, по которой
подключены web-search и repowise; сначала прочитайте [MCP-серверы](README.ru.md).

**Содержание**

- [Перед началом](#перед-началом)
- [Шаги](#шаги)
- [Порядок выката](#порядок-выката)
- [Чеклист](#чеклист)
- [Связанные страницы](#связанные-страницы)

## Перед началом

- **Транспорт:** сервер должен говорить MCP по streamable HTTP (или SSE).
  Серверу только со stdio нужна небольшая HTTP-обёртка в собственном образе.
- **Доверие:** он будет получать от pat-service `X-User-Id`, `X-User-Name` и
  `X-Pat-Token-Id` и может им доверять, только если больше никто до него не
  дотягивается.
- **Данные:** решите, отправляет ли он что-то за периметр компании. Если да,
  ему нужен собственный флаг, который остаётся `false` в air-gap
  установках, и ADR.
- **Стоимость:** инструментам, которые сами вызывают модель (как `get_answer`
  у repowise), нужны более длинный клиентский таймаут и сервисный PAT.

## Шаги

1. **Нагрузка.** Helm-чарт `helm/<name>/` с Deployment и Service в
   `airgap-ai-stack`, образ закреплён в `versions.lock.env` (по digest) и
   NetworkPolicy, которая пропускает входящий трафик на MCP-порт **только**
   от подов с меткой `app.kubernetes.io/name: pat-service`, а исходящий
   ограничивает тем, что серверу нужно. Скопируйте
   `helm/web-search/templates/networkpolicy.yaml`. Цели Makefile
   `<name>-up` / `<name>-down`, перезапускающие Open WebUI, как
   `websearch-up`.
2. **Флаг.** `<NAME>_ENABLED=false` в `.env.example` с комментарием о том,
   что уходит за периметр.
3. **Маршрут в pat-service.** Строка в таблице серверов в
   `pat-service/cmd/pat-service/main.go`
   (`{"<name>", "<NAME>_ENABLED", "<NAME>_MCP_URL", "<URL в кластере по умолчанию>"}`).
   Список имён - это код, а не конфигурация: нужна сборка pat-service (CI
   публикует образ). Добавьте имена инструментов сервера в `mcpToolName` в
   `mcp.go`, чтобы `patsvc_mcp_calls_total` их различал (неизвестные
   считаются как `other`), и тест в `mcp_test.go`. Передайте флаг в
   pat-service: блок env в `k8s/base/applications.yaml` читает его из
   `airgap-runtime`, как `WEB_SEARCH_ENABLED`.
4. **Open WebUI.** Запись в `openwebuiToolServers` в
   `helm/airgap-stack/values.yaml` (`flag`, `id`, `name`, `description`,
   `url: http://pat-service.airgap-ai-stack.svc.cluster.local:8080/mcp/<name>/`).
5. **Агенты.** Запись в `config/agents/base-profile/mcp-servers.yaml`
   (`<name>: {flag: <NAME>_ENABLED, timeout: <секунды>}`). Флаг должен
   дойти до брокера: добавьте его в ConfigMap `agent-images` в цели
   `agents-up` Makefile и в env, который брокер передаёт init-контейнеру
   синхронизации (`agent-broker/cmd/agent-broker/main.go`, `pods.go`), рядом
   с `WEB_SEARCH_ENABLED`. Рендереры покрывает `make agents-test`.
6. **Smoke.** Минимум: `/mcp/<name>/` отвечает на `tools/list` с PAT, 401
   без него, а сервер отказывает в прямом подключении из другого пода.
   Образец - `scripts/websearch-smoke-test`.
7. **Документация.** Таблица серверов и политик в [MCP-серверах](README.ru.md),
   runbook, если нужна эксплуатация, на обоих языках.

## Порядок выката

```sh
# .env: <NAME>_ENABLED=true
make <name>-up                                   # сервер; перезапускает Open WebUI
make helm-up                                     # флаг в airgap-runtime, запись tool server в Open WebUI
kubectl -n airgap-ai-stack rollout restart deploy/pat-service   # если его под не перекатился
make agent-catalog AGENTS_PLATFORM=linux/amd64 && make agents-k3d-load
make agents-up AGENTS_OVERLAY=k8s/overlays/remote-wsl-agents
```

До `helm-up` pat-service должен работать на образе, в котором уже есть
новая строка таблицы, иначе `/mcp/<name>/` так и останется 404.

## Чеклист

- [ ] Чарт, NetworkPolicy (входящий только от pat-service), закреплённый образ
- [ ] Флаг в `.env.example`, выключен по умолчанию
- [ ] Строка в таблице pat-service, имена инструментов, тест, выпущенный образ
- [ ] Запись в `openwebuiToolServers`
- [ ] Запись в `mcp-servers.yaml` и флаг, проброшенный через `agents-up` и брокер
- [ ] Smoke-тест, `make verify`, `make agents-test`, `cd pat-service && go test ./...`
- [ ] ADR, если данные уходят за периметр или вводится новая политика
- [ ] Страницы справочника обновлены на английском и русском

## Связанные страницы

- Назад: [Repowise](repowise.ru.md). Дальше: [Эксплуатация](../operations.ru.md)
- [ADR 0017](../../adr/0017-web-search-mcp-openserp-kagent.md),
  [ADR 0018](../../adr/0018-repowise-codebase-intelligence.md), раздел 5
