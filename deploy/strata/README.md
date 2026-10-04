# Strata (Qwen3.8-Flash-Next)

Third-party inference engine ([github.com/Niko1221/Strata](https://github.com/Niko1221/Strata))
serving Qwen3.8-Flash-Next, a large MoE model, from one GPU plus system RAM.
The model name is **`qwen-3.8-flash-next`**.

**It runs on the GPU host, not in the cluster.** `deploy/strata/run` starts it
as a Docker container on the WSL2 host, and the cluster reaches it as an
external API (handbook pattern A,
[adding an engine](../../docs/handbook/engines/adding-an-engine.md)):
`inference.externalApi` in `helm/airgap-stack/values.yaml` points at
`host.k3d.internal:8095`, rule `openai-external-api`, key `EXTERNAL_API_KEY`.

- No EPP: no queue bands, no per-user fair share. Strata serves **one request
  at a time** behind its own FIFO; the rest wait in it.
- Kubernetes does not see its GPU use. While it runs, every engine replica
  (`VLLM_REPLICAS`, `SGLANG_REPLICAS`, `NINFER_REPLICAS`, `STRATA_REPLICAS`)
  stays 0, or the pod that gets the GPU runs out of VRAM. `STRATA_ON_HOST=1`
  in `.env` lets GPU embeddings start without an engine pod.
- API: OpenAI `/v1/chat/completions` (streaming `usage` included), Anthropic
  `/v1/messages`. Any request model name is accepted.
- No Prometheus metrics (like any `externalApi` backend). Strata's own JSON
  `/metrics` and web Monitor are on the host port.

## Measured

RTX 5090 32 GB, Ryzen 7 7800X3D, 48 GB RAM (WSL2 host), Strata 0.1.37,
IQ3_S, 131K context, INT8 KV, MTP `--spec 4`. Strata's community
`benchmark.py` (fresh prompts, 256 output tokens, greedy, reasoning off),
3 runs per length, on the host:

| Prompt tokens | Prefill tok/s | Decode tok/s | TTFT s |
| ---: | ---: | ---: | ---: |
| 4,096 | 3,701 | 136 | 1.1 |
| 32,768 | 5,371 | 152 | 6.2 |
| 128,000 | 5,141 | 144 | 25.1 |

As deployed (container from `deploy/strata/run`, GPU embeddings running,
the cluster beside it), one request each: ~4.7K prompt 2,264 prefill /
83 decode tok/s, ~38K prompt 4,797 / 116 tok/s. Lower than above because
the VRAM reserve for embeddings leaves fewer experts on the GPU (10,174
instead of 11,752); no expert is read from disk.

## Why not in the cluster

`helm/strata-inference` runs the same image and config as a pod (weights
from MinIO through the node cache, ADR 0019). It works, but on this node it
was 5-6x slower: ~260-420 prefill and ~25-39 decode tok/s.

IQ3_S needs ~55 GB of RAM plus VRAM. With 48 GB of RAM, Strata keeps the
experts the GPU does not hold page-locked in RAM. It sizes that set from
min(host `MemAvailable`, the container's free cgroup memory) minus 4 GiB.
Next to the rest of the stack that left 17.5 GiB of the ~30 GiB it needed,
and the rest was read from `experts.bin` on every request (4.5-10 GB each).

A node with enough RAM for the experts and the stack can run the chart:
`inference.strata.enabled: true`, `inference.externalApi.enabled: false`,
`STRATA_REPLICAS=1`, `STRATA_ON_HOST=0`. Never enable both routes: they
serve the same model name.

## Memory and VRAM on the host

- The container has no memory limit. Strata pins ~28 GiB of experts and the
  host sits near its RAM limit while it runs.
- `--vram-reserve-mib 3772` in `config/strata/strata-iq3_s.json` keeps
  Strata's 700 MiB default plus the GPU embeddings budget
  (`gpu.gpuMemoryBudgetGiB: 3`) free. Strata sizes its expert cache from the
  VRAM that is free when it starts, so start it before GPU embeddings.

## Image

Not published upstream. Built on the GPU host from Strata's own `Dockerfile`
at `STRATA_UPSTREAM_REF` (`versions.lock.env`), for the RTX 50 series only and
without the image encoder:

```sh
# on the GPU host; <ref> is STRATA_UPSTREAM_REF, the tag is STRATA_IMAGE
docker build -t ghcr.io/summerisgone/strata:strata-db4f91a1171d \
  --build-arg CUDA_ARCHITECTURES=120 --build-arg BUILD_VISION=0 \
  "https://github.com/Niko1221/Strata.git#<ref>"
```

`deploy/strata/run` does not use the image's entrypoint (it would run
setup.py and download from Hugging Face). It starts `serve/server.py` on
`config/strata/strata-iq3_s.json`, the config Strata's setup.py writes,
pointed at the model mounted at `/model`.

## Weights

Model `strata-qwen3.8-flash-next-iq3_s` in `models/manifest.yaml`, 127 GB,
also in MinIO (bucket `models`):

| Path | What |
| --- | --- |
| `models/IQ3_S/*.gguf` | the two GGUF shards, ISTA-DASLab revision setup.py pins |
| `packs/iq3_s/` | Strata's native pack: dense weights, tokenizer, `experts.bin` for the low-RAM mode |
| `mtp/rt/` | the MTP draft layer |

On the host it lives in `STRATA_MODEL_DIR` (`config/strata/strata.env`).
To prepare it from scratch, run Strata's own setup and move the three
directories there:

```sh
./setup.sh --yes --family qwen --model IQ3_S --no-start           # in a Strata checkout
# move models/IQ3_S, packs/iq3_s, mtp/rt into $STRATA_MODEL_DIR
```

To restore it from MinIO, copy `models/strata-qwen3.8-flash-next-iq3_s/` from
the bucket and check it with `sha256sum -c` against `models/checksums.txt`.

Check Windows free space before writing 127 GB on the WSL host: its disk is a
growing `ext4.vhdx`, and `df` inside WSL does not show the host drive's limit.

## Running

```sh
# .env: EXTERNAL_API_KEY=<secret>, STRATA_ON_HOST=1, every *_REPLICAS=0
make engines-up                                  # frees the GPU
kubectl -n airgap-ai-stack scale deploy/embeddings-bge-m3 --replicas=0
# on the GPU host:
STRATA_API_KEY=<EXTERNAL_API_KEY> deploy/strata/run
STRATA_API_KEY=<EXTERNAL_API_KEY> deploy/strata/smoke   # first start: up to ~15 min
deploy/vllm-qwen38-nvfp4/k3d-host-dns            # pods resolve host.k3d.internal
# back on the workstation:
make helm-up                                     # route, key, Open WebUI model list
make embeddings-up && kubectl -n airgap-ai-stack scale deploy/embeddings-bge-m3 --replicas=1
```

Start Strata before GPU embeddings: started after them, it sees their VRAM
taken and also keeps its own reserve for them, and holds fewer experts.

`k3d-host-dns` is needed once per cluster, and again if
`host.k3d.internal` stops resolving in pods (k3s rewrites CoreDNS
NodeHosts when nodes change). Envoy keeps a failed lookup for a while:
expect `503 no_healthy_upstream` for up to a minute after the fix.

The container restarts with Docker (`--restart unless-stopped`). Stop it with
`docker rm -f strata` before giving the GPU back to an in-cluster engine.
