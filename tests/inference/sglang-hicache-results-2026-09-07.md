# Результаты первого прогона SGLang HiCache

Дата: 2026-09-07, RTX 5090 на `gpu-host.local`. Тестовый контейнер запускался
на `127.0.0.1:30001`; production Deployment `sglang-qwen38` перед запуском
временно масштабировался до нуля и после прогона восстановлен до одной
готовой реплики. Helm values, маршрутизация, образ, веса и постоянная
конфигурация не менялись.

Методика и критерии: [план HiCache](sglang-hicache-task.md). Клиент нагрузки:
[`sglang-hicache-bench.py`](sglang-hicache-bench.py). Все длинные сессии были
независимыми: nonce помещался в начале каждой истории.

## Итог

На текущем image/checkpoint нет кандидата, который одновременно увеличивает
целевую ёмкость и удовлетворяет требованиям стабильности/скорости:

| Профиль | Результат | Решение |
| --- | --- | --- |
| A0, no-MTP, no offload, HiCache off | Стабилен | Оставить действующий подход исходной точкой |
| W2, `--cpu-offload-gb 2` | Падает при загрузке весов | Не продолжать W1/W4 и не тестировать MTP как компенсацию |
| B8, HiCache 8 GB | Выделяет L2, затем CUDA illegal memory access | Не продолжать B12/B16 и write-policy sweep на этом image |

Причина остановки — не нехватка RAM. Во время B8 после выделения L2 в WSL
оставалось около 25 GiB `MemAvailable`; ошибка была `CUDA error: an illegal
memory access was encountered`. Поэтому остановка прочих Docker-сервисов не
дала бы осмысленного следующего опыта.

## A0: проверенный baseline

Аргументы: no-MTP; context 155648; max-running 2; graph decode bs 2;
`mem-fraction-static=0.88`; `kv-cache-dtype=auto`; Mamba slots 8; FlashInfer;
HiCache и CPU offload выключены.

SGLang выбрал FP8 E4M3 KV и выделил 181956 GPU KV tokens. После CUDA graphs
свободно было 1.82 GiB GPU memory. Short smoke вернул `READY`.

Три прогретых streaming decode по 384 chunks дали:

| Метрика | Значение |
| --- | ---: |
| TTFT, median | 66.3 ms |
| TPOT, median | 14.13 ms |
| TPOT, p95 (по трём прогонам, только ориентир) | 15.04 ms |
| Decode chunks/s, median | 70.05 |
| E2E, median | 5.55 s |

Один независимый input в 60096 фактических tokens вернул маркер за 11.51 s.
Два независимых input примерно по 60k tokens были отправлены через общий
barrier, реально одновременно декодировались (`#running-req: 2`, около 120k
full tokens). Оба получили HTTP 200; лог SGLang показал aggregate generation
throughput около 99 tokens/s после prefill. Их TTFT 11.26 и 22.17 s включают
параллельный длинный prefill и не являются сравнением с коротким decode gate.

## W2: CPU weight offload

Изменён единственный параметр: `--cpu-offload-gb 2`. До построения KV-pool
планировщик завершился ошибкой:

```text
RuntimeError: Expected all tensors to be on the same device,
but found at least two devices, cuda:0 and cpu!
```

Стек указывает на `qwen3_5.py` → `layernorm.py` при загрузке весов. Это
несовместимость текущего SGLang commit `5f55db35e926d50676f75b812640ea2410b0fe0e`
с данным ModelOpt NVFP4 Qwen3.5 checkpoint, а не измеренная потеря скорости.
W1/W4 использовали бы тот же путь и не запускались. Так как базовый CPU
offload не стартует, MTP не может быть проверен в назначенной роли компенсации
offload и намеренно не включался.

## B8: HiCache 8 GB

Изменены только HiCache flags:

```text
--enable-hierarchical-cache --hicache-size 8
--hicache-host-memory-mode cache --hicache-write-policy write_through
--hicache-io-backend kernel --hicache-mem-layout page_first
```

GPU pool не увеличился: 181956 tokens, то есть столько же, сколько у A0.
L2 успешно выделился и был привязан к hybrid cache:

| L2 component | Выделено |
| --- | ---: |
| Attention KV | 218293 tokens, 7.15 GB host RAM |
| Mamba state | 0.86 GB host RAM |

После старта scheduler получил `torch.AcceleratorError: CUDA error: an
illegal memory access was encountered` в обработке первого prefill/decode
batch. HTTP health не стал готовым. Такой профиль не проходит обязательный
startup/correctness gate, поэтому нельзя измерять H2D reload или выдавать
L2 за рабочее расширение активного контекста.

## Следующий обоснованный шаг

Не менять production-профиль. Нужен отдельный короткий compatibility-fix
цикл на более новом SGLang image/commit с тем же checkpoint: сначала A0,
затем W2 и B8, по одному флагу. Только если оба стартуют и проходят short
smoke, возвращаться к 12/16 GB, alternating dialogs и speed gates. Перед
обновлением image нужно сверить upstream release notes и открыть отдельное
изменение image pin; это не было частью данного временного теста.
