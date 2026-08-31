# Clients

Any OpenAI-compatible client works against the public API. It needs two
things: the base URL `https://<public-origin>/v1` and a personal access token
issued from the dashboard at `https://<public-origin>/platform`.

```sh
curl https://<public-origin>/v1/chat/completions \
  -H "Authorization: Bearer sk-…" \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen-3.8-27b","messages":[{"role":"user","content":"hello"}]}'
```

The token is shown once at creation and stored only as an HMAC hash. Revoking
it in the dashboard takes effect on the next request. Requests are rate
limited per token owner, not per token, so issuing several tokens does not
raise the limit.

`model` is a single immutable name, `qwen-3.8-27b`, regardless of which
engine is live underneath. vLLM and SGLang both serve it (the route flag in
`helm/airgap-stack/values.yaml` picks which one answers), so a client never
changes the model name when the operator switches the engine. llama.cpp
(`llamacpp-local`) and an external OpenAI-compatible API are additional
backends behind their own model names — see
[docs/operations/inference-backends.md](../operations/inference-backends.md).
`/v1/models` lists whichever routes are enabled, which is not the same as
which engines are running: the host has one GPU, so vLLM and SGLang take
turns, and a request for the one that is currently down fails. Ask an
operator which is live before switching a client over.

Browser users do not need a token: Open WebUI at `https://<public-origin>/`
signs in through Keycloak and uses the signed-in user's own credentials.

Per-client onboarding notes (Hermes, Pi Agent, OpenCode, KiloCode) belong
here as they are validated.
