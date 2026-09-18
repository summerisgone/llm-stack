# Air-gap

## What is pinned today

`versions.lock.env` is the single source of truth for every image tag and Helm
chart version in the deploy path — the Makefile `include`s it, so a version
cannot be inlined in a recipe. `models/manifest.yaml` and
`models/checksums.txt` hold the model inventory and hashes.

One reference is recorded in two places on purpose: `SGLANG_IMAGE` is also
the `image` value of `helm/sglang-inference`, which is what actually runs.
Update both together.

Two references are not yet digest-pinned:

- the llm-d EPP image, because upstream chart `v0` references the mutable
  `main` tag; its manifest digest at integration time is recorded in
  `versions.lock.env` for mirroring.
- the NVIDIA device plugin, which is a mirrored tag rather than the official
  `nvcr.io` reference because anonymous pulls are not reachable from the
  reference host.
- `PAT_SERVICE_IMAGE`, a first-party image published to
  `ghcr.io/summerisgone/pat-service` by
  `.github/workflows/pat-service-image.yml` on every change. Pin it to a
  `sha-<short sha>` tag or digest once one exists, the same convention as
  `SGLANG_IMAGE`.

## What is missing

An offline bundle procedure: a target that pulls every pinned image and chart,
verifies digests, writes them to a portable archive, and a matching import
step for the target site's registry. Until that exists, an air-gapped install
means mirroring the contents of `versions.lock.env` by hand.

Acceptance evidence for an air-gapped install belongs in `tests/airgap/`.
