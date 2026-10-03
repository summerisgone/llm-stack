# Strata (Qwen3.8-Flash-Next)

Third-party inference engine ([github.com/Niko1221/Strata](https://github.com/Niko1221/Strata))
serving Qwen3.8-Flash-Next, a large MoE model, from one GPU plus system RAM.
Wired as handbook pattern B
([adding an engine](../../docs/handbook/engines/adding-an-engine.md)), like ninfer:
an in-cluster Deployment (`helm/strata-inference`) under its own model name,
**`qwen-3.8-flash-next`**, on the direct gateway rule `openai-strata`.

- No EPP: no queue bands, no per-user fair share. Strata serves **one request
  at a time** behind its own FIFO; the rest wait in it.
- On one GPU it answers only while every other engine is at 0 replicas.
- Its API: OpenAI `/v1/chat/completions` (streaming `usage` included),
  Anthropic `/v1/messages`. Any request model name is accepted.

## Measured

RTX 5090 32 GB, Ryzen 7 7800X3D, 48 GB RAM (WSL2 host), Strata 0.1.37,
IQ3_S, 131K context, INT8 KV, MTP `--spec 4`. Run directly on the host
before the chart, with Strata's community `benchmark.py` (fresh prompts,
256 output tokens, greedy, reasoning off), 3 runs per length:

| Prompt tokens | Prefill tok/s | Decode tok/s | TTFT s |
| ---: | ---: | ---: | ---: |
| 4,096 | 3,701 | 136 | 1.1 |
| 32,768 | 5,371 | 152 | 6.2 |
| 128,000 | 5,141 | 144 | 25.1 |

## Memory

48 GB RAM is below what IQ3_S needs fully in RAM (~62 GB), so the chart runs
setup.py's low-RAM layout: ~11.7K experts (22 GiB) in the GPU cache and the
other ~28 GiB of experts page-locked in RAM (`--resident-experts`, read once
from `packs/iq3_s/experts.bin`). The pod's memory limit is sized for that
(`resources` in `helm/strata-inference/values.yaml`). Expect the host near
its RAM limit while Strata runs.

`vramReserveMiB` keeps 700 MiB (Strata's default) plus the GPU embeddings
budget free: the engine sizes its expert cache from the VRAM free at start,
and `make engines-up` starts embeddings after it.

## Image

Not published upstream. Built on the GPU host from Strata's own `Dockerfile`
at `STRATA_UPSTREAM_REF` (`versions.lock.env`), for the RTX 50 series only and
without the image encoder, then loaded into the cluster:

```sh
# on the GPU host; <ref> is STRATA_UPSTREAM_REF, the tag is STRATA_IMAGE
docker build -t ghcr.io/summerisgone/strata:strata-db4f91a1171d \
  --build-arg CUDA_ARCHITECTURES=120 --build-arg BUILD_VISION=0 \
  "https://github.com/Niko1221/Strata.git#<ref>"
k3d image import ghcr.io/summerisgone/strata:strata-db4f91a1171d -c llm-stack
```

The chart does not use the image's entrypoint (it would run setup.py and
download from Hugging Face). It starts `serve/server.py` on a config the
chart renders (`strata.json` in the ConfigMap), the same file setup.py writes,
pointed at the cached model.

## Weights

Model `strata-qwen3.8-flash-next-iq3_s` in `models/manifest.yaml`, 127 GB:

| Path | What |
| --- | --- |
| `models/IQ3_S/*.gguf` | the two GGUF shards, ISTA-DASLab revision setup.py pins |
| `packs/iq3_s/` | Strata's native pack: dense weights, tokenizer, `experts.bin` for the low-RAM mode |
| `mtp/rt/` | the MTP draft layer |

Prepare it once with Strata's own setup on the GPU host, then move the three
directories under the host model store and upload (ADR 0019):

```sh
./setup.sh --yes --family qwen --model IQ3_S --no-start           # in a Strata checkout
# move models/IQ3_S, packs/iq3_s, mtp/rt into <model store>/strata-qwen3.8-flash-next-iq3_s/
# add their sha256sum lines to models/checksums.txt, then
make helm-up
scripts/models-upload strata-qwen3.8-flash-next-iq3_s
```

The node cache holds another 127 GB copy, filled by the pod's `model-cache`
initContainer.

## Running

```sh
# .env: STRATA_API_KEY=<secret>, STRATA_REPLICAS=1 and every other engine at 0
make helm-up        # strata-api-key Secret, route, Open WebUI model list
make engines-up
```

Metrics: Strata's `/metrics` is JSON, so the `metrics-exporter` sidecar
re-exposes it on `:9400` as `strata_*` (Prometheus job `strata`):
`strata_up`, `strata_requests_total`, `strata_prompt_tokens_total`,
`strata_output_tokens_total`, `strata_prompt_seconds_total`,
`strata_decode_seconds_total`, MTP draft counters, `strata_queued`,
`strata_state`.
