# Задание: увеличить контекст SGLang через CPU offload без существенной потери скорости

Исходное исследование: [RAM и возможности HiCache](../../docs/operations/sglang-hicache-study-2026-09-07.md).
Прочитать его и ADR-0010 целиком. ADR — гипотеза, а не установленный результат.

Результаты прогонов: [исходный image](sglang-hicache-results-2026-09-07.md),
[SGLang 0.5.19](sglang-hicache-results-2026-09-08.md).

## Полномочия и границы

Пользователь явно разрешил тестирование на том же RTX 5090 на gpu-host.local
с временным переключением SGLang. Уточнение пользователя: базовый вариант
**без MTP**; MTP рассматривается только как компенсация замедления CPU offload.
Разделение общего префикса между независимыми сессиями исключено из плана.
Скорость инференса не должна существенно снизиться. Ниже это требование
выражено порогами эксперимента, а не объявлено согласованным SLA.
Работать автономно в этом объёме. Не деплоить победителя постоянно,
не менять маршруты, QoS, драйверы/WSL, веса, фоновые сервисы или секреты.
Не удалять кэши моделей, PVC и данные. Не выполнять docker prune, drop_caches,
swapoff, reboot или wsl shutdown. Не запускать два GPU engine одновременно.

Следовать AGENTS.md и навыку sglang-windows-tuning. Production owner —
helm/sglang-inference; временный host-Docker test container на loopback
допустим, но только после scale-to-zero действующего GPU deployment.
Использовать отдельное имя `sglang-hicache-bench` и свободный loopback port
(предпочтительно 30001), общий ext4 model mount read-only и pinned image.
Не использовать bring-up script вслепую: он может публиковать LAN port.

Доступ: WSL `ssh -p 2222 llmstack@gpu-host.local`; Windows
`ssh llmstack@192.0.2.10` только для наблюдений RAM/commit. Из WSL kubectl:
`docker exec k3d-llm-stack-server-0 kubectl ...`.
Модель `/home/llmstack/models/RadixArk-Qwen3.8-27B-NVFP4`.
Проверить путь по действующему deployment/PV, не скачивать вторую копию.

Перед изменением сохранить replica counts, deployment UID/resourceVersion,
imageID, resolved launch args без API key, baseline health и занятость GPU.
Убедиться, что нет активного теста другого оператора; при изменениях извне
остановить собственные тесты и не перезаписывать чужую конфигурацию.
При активных inference запросах дождаться drain с разумным timeout.
Реализовать rollback через trap/finally **до** остановки baseline: остановить
только свой test container, вернуть исходные replica counts и проверить
readiness, авторизацию, короткий ответ. При SIGKILL trap не поможет:
записать отдельную ручную команду восстановления и текущую фазу на диск.

## Что нужно получить

Найти увеличение активного контекста и/или независимой concurrency при
прохождении ограничений скорости относительно исходного no-MTP профиля.
Основной кандидат — CPU offload весов без MTP; HiCache отдельно проверяется
для сохранения и продолжения независимых длинных диалогов. MTP не является
самостоятельной целью и не оправдывает возврат к короткому контексту.
Показать Pareto-набор среди профилей, прошедших критерии приёмки;
более медленные варианты оставить в таблице отклонённых экспериментов.
Отрицательный результат «HiCache не расширяет active GPU pool» полноценен.
Не пытаться любой ценой подтвердить ожидаемые последствия ADR-0010.

## Этап 0 — собрать воспроизводимый стенд

Сначала изучить существующие внешние helpers для llama.cpp/SGLang-бенчмарков
(вне этого репозитория), в частности test_sglang_long_context.py,
test_sglang_concurrency.py и benchmark_sglang_qwen38.py.
Они не гарантированно безопасны/подходят без адаптации. Создать под
`tests/inference/` runner, manifest экспериментов и инструкцию запуска.
Ошибки проверки должны давать nonzero. Endpoint/ключ передавать конфигом;
секреты не логировать и не включать в JSON артефакты.

Зафиксировать imageID/digest, SGLang git SHA и git diff --stat исходников,
версии PyTorch/CUDA/FlashInfer, tokenizer/model config hash, модель/архитектуру,
фактические resolved args, GPU KV dtype/bytes/tokens, Mamba slots/bytes,
main/draft weights и graph memory. Снять Windows, WSL и cgroup RAM до/после
старта, peak при загрузке и под нагрузкой; polling 1 s достаточно.
Прочитать доступные метрики и сопоставить реальные имена, не выдумывать их.

В установленном SHA 5f55db35e926d50676f75b812640ea2410b0fe0e уже есть FULL+MAMBA
UnifiedRadixCache, разделение hicache-size между pools и MTP draft hooks.
Но startup/реальные запросы должны подтвердить совместимость
extra_buffer_lazy, page-size 64, chunked-prefill 2048, flashinfer, FP8 KV.
Проверить `serve --help` и нормализованные args в тестовом окружении.
При несовместимости зафиксировать точную ошибку; не обновлять image автоматически.

## Этап 1 — baseline и CPU offload без MTP

Точный baseline взять из действующего pod/chart, не из устаревшего env.
Ожидается context=155648, running=3, graph decode bs=3, static=0.88,
KV auto (подтвердить FP8), mamba slots=12, BF16 state,
extra_buffer_lazy, chunk=2048, flashinfer, без speculation. Если текущий pod
отличается, записать различия и воспроизвести согласованный no-MTP baseline.

1. A0: no-MTP baseline, CPU weight offload=0, HiCache off.
2. W2: только `--cpu-offload-gb 2`, MTP off, HiCache off.
3. W4: только увеличение weight offload до 4, если W2 проходит проверки
   памяти и скорости либо требует ограниченного опыта с MTP для компенсации.
4. W1: offload=1 как промежуточный вариант, если W2 уже слишком медленный.
   Большие бюджеты не перебирать до доказанного полезного результата W2/W4.

В этой сборке `cpu_offload_gb` в `utils/offloader.py` умножается на `1024**3`
(GiB), тогда как `hicache_size` — на `1e9` (GB). Это разные механизмы:
weight offload освобождает VRAM под active KV; HiCache хранит reusable KV/state.
Проверить offload смешанных NVFP4/FP8 весов на текущем image, не считать
наличие флага доказательством поддержки. Измерить фактически выгруженные
байты, прирост GPU KV pool, staging buffers, H2D bandwidth и TPOT.
Не приравнивать установленный offload budget к освобождённой VRAM.
При bottleneck переноса допустим один отдельный опыт с поддерживаемым
group/prefetch offloader; сравнить с обычным offload, записать resolved args.

На всех шагах применять раздел «Ограничения скорости». MTP сначала выключен;
после измеренного замедления W-профиля перейти к этапу 3 для проверки
компенсации. Если W-профиль уже удовлетворяет скорости, MTP ему не требуется.

## Этап 2 — HiCache для независимых диалогов

HiCache сначала сравнить отдельно с A0, чтобы отделить его эффект от weight
offload. Увеличение числа сохранённых диалогов не выдавать за active concurrency.

1. A0: исходный no-MTP baseline, weight offload=0, HiCache off.
2. A1: A0 + только page-size=64 при необходимости; HiCache ещё off.
3. B8: A1 + HiCache on, host-memory-mode=cache, size=8 **GB decimal**,
   kernel/page_first, write_through, без storage backend.
4. B12, B16: менять только size на 12, затем 16 GB, если есть смысл и память.
5. На минимальном полезном размере сравнить write_through с write_back,
   затем write_through_selective. По одной оси за раз.
6. Лучший write policy: сравнить kernel/page_first с
   direct/page_first_direct как связанную пару; record effective overrides.
   layer_first/kernel — дополнительный кандидат для L2-only, если сборка
   разрешает и перенос является bottleneck. Не перебирать весь Cartesian grid.

Если отдельно доказана польза HiCache и weight offload, разрешён один
комбинированный кандидат: добавить лучший L2 к лучшему W-профилю, остальные
параметры сохранить. Повторить RAM и speed gates: pinned RAM для весов,
KV/Mamba L2 и staging суммируется, бюджеты не независимы.

Сначала short smoke и 32k, затем длинные тесты. Одно изменение за опыт;
старт с другим лимитом cgroup фиксировать как общий для A/B, чтобы сравнение
не смешивалось с лимитом памяти. Кандидат memory limit 32 GiB требует
проверки общей RAM, request/limit для production не менять в этом задании.

## Наборы нагрузки

Все длины ниже — фактические tokenizer tokens после шаблона. Оставлять
место под output в context cap, проверять usage. Случайные данные/nonce в
начале различают независимые префиксы; не считать повтор одного промпта
независимыми сессиями. Не конструировать общий длинный префикс и не
оптимизировать межсессионное sharing. Повторное использование истории
внутри одного диалога оставлено для проверки HiCache.
Фиксированные seeds; одинаковые данные между A/B.
Измерять минимум три повтора прошедшего кандидата, cold отдельно от warm.
Первичный coarse screening можно сделать по одному разу.

| Набор | Проверка |
| --- | --- |
| Short | health/models, valid/invalid auth, точный ответ, reasoning/tool parsing |
| Single cold | input 32k → 64k → 96k → 128k → 148k; 512 output reserve |
| Independent active | 2×64k, 3×48k; затем 2×80k, 2×96k, 3×64k, при успехе 2×128k/3×96k |
| Alternating dialogs | 4, 6, 8 независимых диалогов по 64k, затем 96k/128k; active=1 или 2, несколько раундов продолжения |
| Mixed | один длинный prefill + уже идущие 1–2 decode; оценка stalls/fairness |
| Pressure/recovery | достаточно разных префиксов для вытеснения L1, затем возврат к первому с новым суффиксом |

Независимая concurrency: одновременный старт через barrier, streaming,
256–512 фактически сгенерированных tokens для устойчивого overlap.
Для performance можно ignore_eos, если поддерживается; correctness отдельно
без принудительного продолжения. Записать per-request timestamp первого/
последнего токена и интервалы decode, running/queued/retracted.
HTTP 200 двух параллельно отправленных запросов не доказывает два активных
декодера, если scheduler исполнил их по очереди.

Сохранённый L2 доказывать ростом H2D/load-back bytes/tokens после вытеснения
GPU, host hit metrics и уменьшением вычисленного prefill. Общий cached_tokens
не отличает GPU hit от RAM hit. Не использовать flush_cache между вытеснением
и возвратом: он может стереть L2. Проверка correctness: разные маркеры в
начале/середине/конце и многотуровые ветвления, отсутствие чужого префикса,
сравнение ответов с cold baseline. Короткий ответ с одним маркером недостаточен
для performance измерения длинного decode.

Один запрос выше 155648: только если измеренный GPU pool действительно
позволяет input+output, в отдельном опыте context-length=163840/196608 и далее
не выше нативного 262144. Если пул меньше, не запускать обречённый sweep:
зафиксировать GPU bound. L2 bytes не прибавлять к active token capacity.

## Этап 3 — MTP только для компенсации CPU offload

Только после измеренного замедления CPU weight offload. Матрица для одного
перспективного W-профиля, сначала с HiCache off:

| Профиль | CPU weight offload | MTP |
| --- | --- | --- |
| A0 | 0 | off — основной эталон |
| W | выбранный бюджет | off — измеренная цена offload |
| WM | тот же бюджет | on — проверка компенсации |

MTP должен приблизить скорость WM к **A0**, а не просто ускорить медленный W.
Профиль WM принимается только при прохождении speed gates и сохранении
заявленной увеличенной формы контекста/concurrency. Ускорение на 16k не
компенсирует утрату достигнутого длинного контекста. Если MTP не нужен для
прохождения gates или не даёт полезной компенсации, оставить его выключенным.

Использовать встроенную MTP head текущего checkpoint, не внешний DSpark/DFlash
checkpoint. В историческом профиле EAGLE: steps=3, topk=1, draft-tokens=4;
сначала проверить поддержку в текущей сборке и воспроизвести на безопасном
8k/16k input с active=1 и graph bs=1, затем 24k/32k по реальному пулу.
Начальные 8k/16k — только smoke/compatibility, не итоговое сравнение скорости.
Контроль no-MTP для сравнения должен иметь те же graph/concurrency,
context cap, prompt/output и cache state. Основные speed gates всегда
сопоставлять также с A0, чтобы общий вред offload не скрывался промежуточным W.

Записать MTP head bytes, main/draft KV, state/graphs, acceptance rate,
accepted length, speculative overhead и потери GPU pool. Исторические
5.71 GiB и 33208 tokens — не подставлять вместо измерения.
Если есть полезное ускорение, ограниченно сравнить поддерживаемые
(steps=1, draft=2) и (steps=2, draft=3) с (3,4), topk=1; фиксировать связанные
изменения и совместимость. Далее concurrency=2 и 3 только в пределах пула.
Не включать NVFP4 KV вместе со speculation; NVFP4 weights не равны NVFP4 KV.

При pool около 33k **не утверждать**, что 8–16 GB L2 вернули активный 150k.
Отклонить такой WM для цели длинного контекста и вернуть MTP off;
не переключать задачу на оптимизацию коротких быстрых диалогов.
Если L2 позволяет уменьшить GPU Mamba checkpoint slots, отдельный опыт
12→8 (или другой подтверждённый runtime минимум) с сохранением нужного
active count; не предполагать, что host state заменяет обязательные active
и speculative slots. Тестировать branches/resume correctness после изменения.

## Этап 4 — только обоснованные донастройки

На одном-двух лучших профилях chunked-prefill 1024/2048/4096 для mixed load.
Static memory менять шагом 0.005 только если есть безопасный запас VRAM;
сейчас исторически меньше 1 GiB, поэтому повышение не первый шаг.
Каждое изменение перепроверить на достигнутом длинном контексте/concurrency.
Если MTP требует квантования весов, FP4 KV или изменения архитектуры,
оформить отдельное предложение; в текущий sweep не подмешивать изменение
качества. Отключение vision также не входит в этот план без отдельного решения.

## Ограничения скорости — обязательный критерий приёмки

Рабочее определение «скорость не должна сильно упасть»: на одинаковых
нагрузках относительно A0 допускается **не более 10% потери decode throughput**.
Пороги выбраны для этого плана как консервативная трактовка пожелания пользователя:

| Метрика на равной нагрузке | Порог относительно A0 |
| --- | --- |
| Per-session decode tokens/s, median для каждой формы нагрузки | ≥90% |
| Aggregate output tokens/s при той же concurrency | ≥90% |
| Median TPOT | ≤110% |
| p95 per-request TPOT на достаточной выборке | ≤120% |
| Median TTFT и E2E, каждый отдельно | ≤115% |

Все gates обязательны: aggregate throughput не оправдывает медленную
отдельную сессию, тёплый L2 hit не маскирует деградацию cold prefill.
Сравнивать одни и те же независимые prompts, tokenizer input/output lengths,
concurrency, sampling, режим streaming, прогрев и состояние cache.
Отдельно считать cold, warm/resume и mixed. Baseline снимать до серии и
повторять для финалиста; при >5% расхождении повторов baseline выяснить
фоновую нагрузку и повторить сравнение. Результат у границы или в пределах
шума считать неопределённым до повторных измерений, не округлять в pass.

На увеличенном контексте/concurrency, который A0 не вмещает, нет прямого
равного baseline. Для него записать speed comparison=N/A, абсолютные
TTFT/TPOT/tokens/s и форму нагрузки. Вывод о ≤10% overhead разрешён только
для общей области работоспособности; нельзя сравнивать 150k с 32k или
1 сессию с 3 и объявлять offload без замедления. Для новой области показать
отдельную кривую latency/throughput по длине и числу сессий; при отсутствии
абсолютного SLA не объявлять её скорость автоматически приемлемой.

Если после прогрева и трёх сопоставимых измерений W теряет >25% decode
скорости, прекратить увеличение offload: проверить меньший бюджет или
один ограниченный опыт с MTP/prefetch для компенсации. Не расходовать
всю серию на заведомо медленные варианты. Если ни один кандидат одновременно
не даёт прироста ёмкости и не проходит speed gates, итог — «на текущем
железе приемлемый компромисс не найден» с таблицей причин, без ослабления порогов.

## Ограничители и завершение

HiCache allocator этой сборки резервирует 10 GiB host available.
После allocation поддерживать WSL MemAvailable ≥10 GiB, Windows free ≥4 GiB;
не обходить allocator guard. Контролировать Windows virtual/commit headroom,
cgroup memory.current/max/peak/events и anon/file отдельно. При запасе
cgroup <2 GiB либо >90% limit остановить увеличение нагрузки и оценить
reclaimable file: одно лишь file usage не равно OOM, но ignored OOM недопустим.
Постоянный новый swap-in/out или memory PSI full avg10 >1% в 3 последовательных
5-секундных проверках — остановка кандидата.
CUDA/DXG/Xid/OOM/SIGBUS, неправильный маркер, утечка prefix или disconnect —
fail, не продолжать sweep этого профиля. Низкую VRAM (<1 GiB) явно помечать
как хрупкую конфигурацию даже если baseline тоже таков.

Каждый запрос timeout 300 s, запуск 900 s; повтор одного и того же failed
кандидата максимум один раз после диагностики. Первая серия не больше
12 materially distinct конфигураций: выбрать информативные опыты по результатам.
Финалистов проверить ≥3 повторов и 30 min mixed/alternating soak.
Промежуточные результаты сохранять после каждого опыта, не только в конце.

Метрики: input/output/unique-prefix tokens, TTFT, prefill tok/s, per-session
TPOT (median/p95), aggregate output tok/s, E2E, queued time, реальный overlap,
retractions, errors, GPU/RAM high-water marks, L1/L2 hits/transfer bytes,
MTP acceptance. p95 из трёх наблюдений не выдавать за надёжный tail estimate;
для tails собрать ≥30 запросов на финалистах в soak.

Критерии выбора: correctness и отсутствие OOM/retraction для заявленной
активной ёмкости; отдельно максимальная проверенная форма нагрузки.
Обязательны все пороги раздела «Ограничения скорости». Для более крупного
L2 дополнительно требовать измеримый выигрыш (ориентир ≥10% TTFT в alternating
workload). Для MTP критерий — компенсация CPU offload до порогов A0 при
сохранении увеличенной ёмкости. Самостоятельный MTP speedup не является целью.
Это критерии эксперимента, а не пользовательский SLA.

Артефакты: machine-readable JSON/CSV по каждому request/profile, sanitised
startup/error logs и metrics snapshots, воспроизводимый runner,
`tests/inference/sglang-hicache-results.md` с таблицей Pareto, измеренным
RAM budget, рекомендуемыми flags, ограничениями и предлагаемыми правками
draft ADR-0010. Постоянный rollout и принятие ADR не входят в задачу.
Результат «не поддерживается/не помогает» сопровождать точным evidence.

После тестов восстановить исходный engine и подтвердить health/auth/короткий
инференс. В финале явно написать что восстановлено, какие файлы созданы,
что доказано, что осталось гипотезой. Если менялись manifests/chart,
выполнить `make verify`, `helm template sglang helm/sglang-inference -n airgap-ai-stack`,
client dry-run и релевантный smoke; при docs/scripts-only запускать лишь
релевантные проверки runner, не разворачивать весь stack.
