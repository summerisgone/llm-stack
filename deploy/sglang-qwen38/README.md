# SGLang Qwen3.8 27B NVFP4

Runs the same `RadixArk-Qwen3.8-27B-NVFP4` checkpoint as
`deploy/vllm-qwen38-nvfp4` through SGLang instead of vLLM, as a plain Docker
container directly on the WSL2 host. Nothing here requires a rebuild: the
image is `lmsysorg/sglang:v0.5.19`, pinned by digest in `versions.lock.env`
and used as the fallback in `deploy/sglang-qwen38/run`.

**This is the bring-up path, not the deployed one.** SGLang runs in the
cluster as its own Helm release (`helm/sglang-inference`, `make sglang-up`),
which is what `helm/airgap-stack/values.yaml` `inference.sglang.host` points
at -- see
[ADR 0007](../../docs/adr/0007-inference-engines-as-helm-releases.md). Use the
scripts here to try flags on the host, benchmark outside Kubernetes, or run
the engine on a machine with no cluster.

**Single GPU**: this host has one RTX 5090. `vllm-qwen38-nvfp4` already holds
it inside Kubernetes; running this at the same time contends for the same
card, invisibly to the cluster scheduler. Scale vLLM to zero first:

```sh
kubectl -n airgap-ai-stack scale deployment/vllm-qwen38-nvfp4 --replicas=0
```

## Run and validate on the WSL2 host

```sh
./run
./smoke
```

Both read defaults from `config/sglang/qwen38.env`; override any of them
from the environment, e.g. a different port:

```sh
PORT=30001 ./run
```

`./run` requires the API key file it points at
(`API_KEY_FILE`, default `/home/llmstack/.config/sglang/api_key`) to already
exist and be non-empty -- generate one the same way the host's own
`/home/llmstack/sglang-qwen38/scripts/ensure_sglang_api_key.sh` does, or reuse
that file directly if it is already there.

## Wiring a host-run container into the AI Gateway

The route already exists: `inference.sglang.enabled` is `true` and the model
is selectable as `qwen38-nvfp4-sglang` through the same `/v1` route vLLM's
`qwen38-nvfp4` uses. It points at the in-cluster Service. To serve it from
this host container instead, repoint the host:

```sh
helm upgrade --install airgap-stack helm/airgap-stack \
  --namespace airgap-ai-stack --values helm/airgap-stack/values.yaml \
  --set inference.sglang.host=host.k3d.internal \
  --set runtimeEnvironment.SGLANG_API_KEY="$(cat /home/llmstack/.config/sglang/api_key)"
```

`host.k3d.internal` is k3d's own DNS alias for the Docker bridge gateway on
this host (`172.21.0.1` on the current cluster); nothing site-specific needs
to be recorded because k3d provides that alias on every cluster it creates.
Put the value in `values.yaml` rather than `--set` if the host container is
how the site actually runs SGLang.
