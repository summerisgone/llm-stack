# Повторный прогон SGLang 0.5.19 и диагностика HiCache

Дата: 2026-09-08. Хост: RTX 5090 на `gpu-host.local`. Production SGLang на время
каждого опыта масштабировался до нуля; тестовый сервер слушал только
`127.0.0.1:30001`. Постоянная Helm-конфигурация не менялась. Полные журналы и
JSON результатов сохранены на стенде в
`/home/llmstack/sglang-hicache-results-latest/`.

Использован официальный stable image `lmsysorg/sglang:latest`, фактически:

```text
SGLang 0.5.19
commit 0bcd822377da7b5718e674eaf9c870d349424dd1
image digest sha256:d6e7288627be8b02be88e4bba38e73f6d50e2826869f753c13a4c4385ab3eda9
PyTorch 2.13.0+cu130, CUDA 13.0
```

## Результат

HiCache для этого hybrid Qwen3.5 ModelOpt NVFP4 checkpoint остаётся
неработоспособным: все поддерживаемые варианты повреждают CUDA memory на первом
inference/warmup. CPU weight offload также не стартует. Рабочий способ увеличить
одиночное контекстное окно найден без CPU offload, HiCache и MTP: поднять
`mem-fraction-static` с 0.88 до 0.90 и `context-length` до 180224.

| Профиль 3/12 | GPU KV pool | Context cap | Свободно после graphs | Decode median |
| --- | ---: | ---: | ---: | ---: |
| A0: static 0.88 | 172380 | 155648 | 1.79 GB | 69.10 chunks/s |
| C90: static 0.90 | 191972 | 180224 | 1.22 GB | 68.34 chunks/s |

C90 увеличивает GPU KV pool на 19592 токена, или 11.4%, и объявленный контекст
на 24576 токенов, или 15.8%. На одинаковом коротком decode медианная скорость
снизилась на 1.1%, median TPOT вырос с 14.43 до 14.55 ms (+0.8%). Это проходит
порог плана: не более 10% потери decode throughput.

Проверка C90 в production-форме (`max-running-requests=3`, Mamba slots 12,
decode graph bs 3) успешно обработала 170094 фактических входных токена за
61.16 s, вернула полный уникальный маркер и HTTP 200. Три независимых запроса
примерно по 60k также получили HTTP 200, но scheduler держал два active и один
queued. Поэтому опыт доказывает увеличение одиночного окна, но не увеличение
одновременной long-context concurrency до трёх.

Дополнительная форма 2/8 с теми же `0.90/180224` выделила 201548 GPU KV tokens,
обработала два независимых input примерно по 90k и показала median decode
69.16 chunks/s. Она полезна как отдельный двухсессионный профиль, но уменьшает
текущий лимит production с трёх запросов до двух и потому не является прямой
заменой.

## Локализация CUDA illegal memory access

В B8 выделялись 7.15 GB host RAM для 218293 attention KV tokens и 0.86 GB для
Mamba state. GPU KV pool при этом не рос. С `CUDA_LAUNCH_BLOCKING=1` ошибка
локализована в выгрузке Mamba-state GPU → host:

```text
MambaPoolHost.backup_from_device_all_layer
  -> transfer_kv_mamba_lf_pf
  -> transfer_mamba.cuh:184
RuntimeError: CUDA error: an illegal memory access was encountered
```

В 0.5.19 уже присутствует исправление размещения `dst_indices` на CUDA,
добавленное после upstream issue
[#24121](https://github.com/sgl-project/sglang/issues/24121), однако на RTX 5090
SM120 этого недостаточно. Проверены и отклонены:

- `kernel + page_first + write_through`: детерминированный crash первого batch;
- `direct + page_first_direct`: тот же illegal access;
- оба варианта с `CUDA_LAUNCH_BLOCKING=1`: подтверждено раннее повреждение CUDA
  context в host transfer path;
- `extra_buffer + disable-overlap-schedule`: также падает; следовательно,
  overlap scheduler не является причиной;
- `extra_buffer_lazy + disable-overlap-schedule`: SGLang корректно отвергает
  эту несовместимую пару до запуска.

Это не host OOM. В предыдущем B8 после выделения L2 оставалось около 25 GiB
`MemAvailable`; в этом прогоне allocator также успешно создал оба host pools.
Остановка остальных Docker-контейнеров не требовалась и не исправила бы выход
CUDA-ядра за границы.

## CPU offload и MTP

`--cpu-offload-gb 2` на 0.5.19 повторяет ошибку старой сборки при загрузке
ModelOpt NVFP4 весов:

```text
RuntimeError: Expected all tensors to be on the same device,
but found at least two devices, cuda:0 and cpu!
```

Поскольку W2 не доходит до KV allocation, W1/W4 не дают нового сигнала, а MTP
нечего компенсировать. MTP намеренно не включался: C90 уже проходит speed gate,
а MTP уменьшил бы доступный GPU KV pool и противоречил бы цели длинного
контекста.

## Рекомендация

HiCache не включать для этого checkpoint до upstream fix с отдельным тестом
hybrid Mamba на SM120. Не использовать CPU weight offload и MTP в production.

Для следующего отдельного изменения проверить и затем закрепить digest SGLang
0.5.19 вместе с:

```text
--context-length 180224
--mem-fraction-static 0.90
--max-running-requests 3
--cuda-graph-max-bs-decode 3
--max-mamba-cache-size 12
```

Перед постоянным rollout нужен более длинный soak и mixed-load тест: запас
1.22 GB после CUDA graphs меньше baseline на 0.57 GB. Если при soak появится
runtime OOM, консервативный промежуточный шаг — static 0.895 с повторным
измерением pool, а не CPU offload или HiCache.
