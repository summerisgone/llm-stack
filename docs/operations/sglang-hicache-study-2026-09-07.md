# SGLang HiCache: RAM, active context and MTP

Исследование 2026-09-07, примерно 23:05–23:18 (локальное время).
Статус: read-only диагностика завершена; параметры на сервере не менялись,
инференс для исследования не запускался. Пользователь разрешил отдельной
задаче временно переключать профиль на том же RTX 5090. Последующее уточнение:
baseline без MTP; MTP нужен только как возможная компенсация замедления CPU
offload. Общие префиксы исключены из плана. Скорость не должна сильно падать.
План исполнения: [задание на стенд](../../tests/inference/sglang-hicache-task.md).

## Вывод для ADR-0010

HiCache стоит испытать для большого числа сохраняемых длинных диалогов и
ускорения их продолжения. Он не превращает RAM в дополнительный GPU KV-пул
для dense attention: нужный префикс перед вычислением возвращается на GPU.
Поэтому нельзя обещать ни один запрос сверх физического GPU-пула, ни
2–3 независимых одновременно декодируемых контекста по 155k только за счёт L2.
Общий префикс может физически разделяться; это отдельный сценарий.

Основания: [официальное описание workflow](https://docs.sglang.io/docs/advanced_features/hicache_design)
и прочитанный код работающего образа: `unified_radix_cache.py`,
`_load_back_transfers()` (около строк 1398–1452) сначала проверяет квоту,
затем свободные GPU-слоты и eviction, затем вызывает H→D `load()`.
Невозможность выделения приводит к отказу восстановления, а не к attention
непосредственно из CPU RAM. Это особенно существенно для MTP: сохранённый
в RAM префикс не устраняет расходы GPU на голову и draft/state/graphs.

ADR-0010 сейчас proposed/draft, менять его статус до эксперимента нельзя.
Следует пересмотреть его ожидаемые последствия и следующие детали:

- HiCache у данного гибрида хранит attention KV **и Mamba state**, а не только KV.
- `--hicache-size` — десятичные GB (`1e9`), поэтому имя `hicacheSizeGiB`
  без преобразования единиц ошибочно. Предпочтительно `hicacheSizeGB`.
- Уже существует `/dev/shm` emptyDir 16 GiB в SGLang chart. Это потолок
  tmpfs, а не зарезервированные 16 GiB. Сейчас занято около 33 MiB.
  HiCache использует host tensors с pinning и не требует аналогичного
  vLLM mmap-файла на всю величину L2.
- Ныне vLLM не запущен; его возможные 8 GiB не надо вычитать одновременно
  с SGLang. Взаимоисключение движков нужно сохранить.
- Приведённые в ADR «~228k выше 230104» арифметически неверны.
  Рост reload bytes подтверждает перенос, но сам по себе не доказывает
  одновременно резидентный рабочий набор сверх пула; нужны временная шкала,
  точные input/output и cache hits. Запись vLLM здесь не переиспытывалась.

## Реальная память хоста

WSL: SSH `llmstack@gpu-host.local:2222` (Linux/fish). Windows: SSH
`llmstack@192.0.2.10:22` (PowerShell). Interop-вызов PowerShell из WSL
вернул Invalid argument; Windows измерена через её собственный SSH.
GiB ниже = 2^30 bytes. Снимки сделаны последовательно, а не атомарно.

| Граница | Наблюдение |
| --- | --- |
| Windows физическая RAM | 63.05 GiB, свободно 7.79 GiB |
| Windows virtual memory по CIM | всего 107.05 GiB, свободно 15.33 GiB; это не физическая RAM |
| `vmmemWSL` working set | 45.82 GiB; PrivateMemorySize около 79.37 GiB не является resident RAM |
| Windows другие крупные процессы | Memory Compression 1.08 GiB; jellyfin 0.27; крупнейший LM Studio 0.22; explorer 0.22 GiB |
| WSL MemTotal | 47.86 GiB |
| WSL used / MemFree / MemAvailable | 18.15 / 0.92 / 29.71 GiB |
| WSL buffers + cache | 29.62 GiB, значительная часть может быть вытеснена |
| WSL AnonPages | 16.22 GiB |
| WSL Cached / Buffers / SReclaimable | 25.28 / 2.67 / 1.67 GiB; это компоненты/пересекающиеся представления, не прибавлять к used |
| WSL swap | 1.47 из 8 GiB; в двух секундных интервалах vmstat si/so = 0 |
| WSL memory PSI | some/full avg10/60/300 = 0 в момент снимка |
| `.wslconfig` | `memory=52428800000`, `swap=8GB`, `vmIdleTimeout=-1`, sparseVhd=true |
| GPU | RTX 5090, total 32607 MiB, used 31645 MiB, free 543 MiB по nvidia-smi; учёт reserved объясняет несовпадение суммы |

Главный потребитель физической RAM Windows — WSL. Повышение лимита WSL
пока не требуется: внутри уже есть reclaimable cache. Свободную RAM Windows
и WSL MemAvailable складывать нельзя — это вложенные уровни учёта.
Сброс page cache, swapoff, изменение WSL или остановка фоновых сервисов
для этих экспериментов не нужны.

### Внутри WSL

`docker stats` показывает k3d node 28.16 GiB, но это **включает** Kubernetes
pods; нельзя суммировать эту строку с `kubectl top pods`.
Основные working set по `kubectl top`: SGLang 11903 MiB, ClickHouse 1497,
Langfuse worker 990, Open WebUI 963, Keycloak 906, Langfuse web 898,
monitoring Prometheus 878 MiB. Есть отдельные Prometheus/Grafana в
`monitoring` и `airgap-ai-stack`.

Помимо k3d работают host-Docker Open WebUI, Langfuse web/worker,
ClickHouse, Postgres, Redis, MinIO, Hermes и sglang-prometheus.
Это кандидаты для отдельного аудита назначения, не доказанные ненужные копии.
У host MinIO docker stats показывает 3.734 GiB, тогда как RSS процесса
около 171 MiB: статистику контейнера нельзя целиком считать анонимной
невытесняемой памятью. Host ClickHouse — около 1.06 GiB по docker stats;
Open WebUI 598 MiB; Langfuse web/worker 469/358 MiB; Hermes 414 MiB.

SGLang cgroup (`/sys/fs/cgroup` внутри pod):

| Показатель | Значение |
| --- | --- |
| memory.current / memory.max | 14.50 / 24 GiB |
| memory.peak | 18.33 GiB за жизнь cgroup; не только текущая нагрузка |
| anon / file / kernel | 5.99 / 8.34 / 0.18 GiB |
| inactive_file / active_file | 2.87 / 5.21 GiB |
| shmem | 0.255 GiB, входит в file |
| memory.events max / oom / oom_kill | 0 / 0 / 0 |

Разница с `kubectl top` ожидаема: working set исключает inactive_file,
а memory.current включает его. Сумма RSS процессов также завышает
некоторые shared mappings. Из этих данных нельзя заключать, что SGLang
уже безвозвратно занял 14.5 GiB RAM.

## Проверка установленной реализации

Chart pin:
`lmsysorg/sglang:dev-qwen38-27b-dflash2@sha256:616a3e97f45191af975896cfa644279096cb31bd408a071c2e99ca7209c3cafe`.
В работающем pod `git rev-parse HEAD` для `/sgl-workspace/sglang`:
`5f55db35e926d50676f75b812640ea2410b0fe0e`. Перед тестом всё равно записать
runtime imageID, локальные изменения исходников и фактические resolved args.

Прочитаны файлы под `/sgl-workspace/sglang/python/sglang/srt/`:

- `server_args.py`: enable-hierarchical-cache, host-memory-mode cache,
  размер/ratio, три write policies, kernel/direct, layouts присутствуют.
  Это проверка исходников, не доказательство успешного запуска HiCache.
- `mem_cache/registry.py`: hybrid SSM выбирает `UnifiedRadixCache`,
  компоненты FULL + MAMBA, затем `init_hicache()`.
- `mem_cache/hybrid_cache/hybrid_pool_assembler.py:700`: абсолютный бюджет
  делится между KV и Mamba пропорционально device pool bytes. Есть ветка
  подключения MTP draft pools; её корректность на этой модели ещё не проверена.
- `mem_cache/pool_host/base.py`: `host_size * 1e9`, pinning; бюджет проверяется
  относительно MemAvailable с резервом **10 GiB**. Маленький L2 относительно
  GPU здесь вызывает warning, хотя опубликованные docs требуют большего L2.
  Для exact build первичен код плюс реальный запуск.
- `server_args.py`: HiCache несовместим с int8 Mamba checkpoints и
  `enable-unified-memory`; buffer_only не поддерживает Mamba.
  `registry.py` также отвергает host-pool decode retraction для Mamba.

Известный upstream [дефект двойного расхода RAM](https://github.com/sgl-project/sglang/issues/29034)
исправлен [PR 32915](https://github.com/sgl-project/sglang/pull/32915), merged
2026-07-31. В проверенном коде разделение бюджета присутствует; всё равно
измерять сумму реально выделенных pools, особенно с MTP и округлением страниц.

`/model/config.json`: architecture `Qwen3_5ForConditionalGeneration`,
64 слоя, 16 full-attention + 48 linear-attention, 4 KV heads, head_dim 256,
max_position_embeddings 262144, одна MTP layer. Имя маршрута Qwen3.8 не
означает другое имя Python architecture. Для FP8 full-attention KV:
`2 * 16 * 4 * 256 * 1 = 32768 bytes/token` до padding/scale/allocator overhead.
При FP16 это вдвое больше. 150000 tokens — около 4.58 GiB только main KV.
Mamba states и MTP учитывать отдельно.

## Предлагаемая схема

Актуальный порядок по уточнению пользователя: no-MTP baseline → CPU weight
offload 2/4 GiB без MTP → при замедлении проверить MTP для компенсации.
Weight offload переносит часть весов в RAM, освобождая VRAM для active KV;
он отличается от HiCache. Поддержку смешанного checkpoint и фактический
прирост GPU pool нужно измерить. В прочитанном `utils/offloader.py` бюджет
`cpu_offload_gb` пересчитывается через `1024**3`, в отличие от decimal HiCache GB.
Объём offload не равен гарантированному приросту свободной VRAM из-за staging.

GPU: оставшиеся main weights + active KV/state + graphs/workspaces;
MTP добавляется только для компенсации измеренного замедления offload.
RAM: L2 cold reusable KV/state → при новом ходе load-back в GPU.
RAM для выгруженных весов и L2 нужно учитывать совместно.
Дисковый L3 пока не нужен. Не вводить отдельный постоянно живущий GPU server.

В отдельной проверке HiCache начать с 8 GB (7.45 GiB), page-size 64, kernel/page_first,
write_through, без MTP. Если baseline page-size иной, предварительно снять
baseline с page-size 64, чтобы не смешивать эффект HiCache и размера страниц.
Далее 12 GB (11.18 GiB), 16 GB (14.90 GiB) только при полезном приросте hits/TTFT.
Это кандидаты, а не уже безопасные или оптимальные значения.

Оставлять после pinning минимум 10 GiB WSL MemAvailable, Windows free
не менее 4 GiB, контролировать commit и отсутствие нового paging.
Бюджет L2 ограничивать минимумом доступного host headroom и cgroup headroom
после учёта измеренного нереклеймируемого пика, transient allocations и резерва.
Пиковая загрузка весов должна учитываться отдельно от steady state.
На тестовом контейнере разумный начальный memory limit 32 GiB; этого не
считать доказательством вместимости 16 GB L2. На 24 GiB могут понадобиться
reclaim/reload файлов, а большие L2 могут привести к cgroup OOM.

Искомые результаты различать:

1. Максимальный проверенный один **активный** контекст.
2. Одновременно декодирующие независимые длинные запросы.
3. Много длинных диалогов с паузами: сохранение в RAM и быстрое продолжение.
4. Компенсация замедления CPU offload через MTP при сохранении достигнутой
   увеличенной ёмкости. Общие префиксы между сессиями не тестируются.

В плане введён рабочий порог: decode throughput ≥90% исходного no-MTP
baseline, median TPOT ≤110%, p95 TPOT ≤120%, median TTFT/E2E ≤115% на
одинаковой нагрузке. Это предложенная численная трактовка пожелания
«не сильно упасть», не ранее согласованный SLA. Полные условия сравнения
и обработки шума — в задании. Для контекста, который baseline не вмещает,
прямое сравнение отсутствует: нужны абсолютные скорости и отдельный вывод.

Историческая запись skill от 2026-08-25: без MTP pool 172380 tokens,
проверено 1×148036 / 2×74036 / 3×49036; с EAGLE MTP дополнительные
~5.71 GiB и pool ~33208. Это ориентир для перепроверки, не текущий результат.
Если MTP снова оставит только ~33k GPU tokens, HiCache сам по себе не
вернёт активные 150k. Такой MTP-кандидат отклоняется для поставленной цели;
задачу не переключать на короткий быстрый decode.
Изменение dtype/весов/квантования и HiSparse не включать в основную матрицу:
это другие механизмы, с отдельной проверкой качества.

## Источники и воспроизведение диагностики

Кроме указанных ссылок: [best practices](https://docs.sglang.io/docs/advanced_features/hicache_best_practices),
[hybrid Mamba branching PR 31181](https://github.com/sgl-project/sglang/pull/31181).
Текущие online docs не заменяют проверку pinned image.

Наблюдения получены через `free -b`, `/proc/meminfo`, `/proc/pressure/memory`,
`vmstat 1 3`, `ps -eo pid,ppid,comm,rss --sort=-rss`, `docker stats --no-stream`,
`kubectl top pods -A --sort-by=memory`, cgroup memory.* внутри SGLang,
`df -h /dev/shm`, `nvidia-smi`, Windows CIM Win32_OperatingSystem и
`Get-Process` с ограниченным набором числовых полей. Секреты не читались.
