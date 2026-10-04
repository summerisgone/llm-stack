# Agent images for the pi, opencode, dsh and hermes runtimes behind
# agent-broker: the agent plus agent-adapter, which serves the broker's agent
# contract on :8642. One target per runtime, built by `make agent-adapter-images`:
#   docker buildx build --target pi --tag llm-stack/agent-pi:<tag> agent-adapter
# The pod runs it as uid 10000 with a read-only root filesystem; the profile
# PVC, /work and /tmp are the writable paths (agent-broker pods.go).
FROM node:22.23.1-bookworm-slim@sha256:6c74791e557ce11fc957704f6d4fe134a7bc8d6f5ca4403205b2966bd488f6b3 AS base
# ripgrep: opencode's grep/glob tools, which would otherwise download it.
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ripgrep \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /opt/agent
COPY package.json server.mjs acp.mjs ./
ENTRYPOINT ["node", "/opt/agent/server.mjs"]

FROM base AS pi
ARG PI_VERSION=0.87.1
ARG PI_MCP_ADAPTER_VERSION=2.37.0
# pi-acp: pi over ACP (acp.mjs, AGENT_PROTOCOL=acp, docs/adr/0021); it runs
# `pi --mode rpc` through pi-rpc.
ARG PI_ACP_VERSION=0.0.34
RUN npm install --omit=dev --no-audit --no-fund \
      @earendil-works/pi-coding-agent@${PI_VERSION} pi-mcp-adapter@${PI_MCP_ADAPTER_VERSION} \
      pi-acp@${PI_ACP_VERSION} \
 && npm cache clean --force
COPY pi.mjs pi-rpc ./
# No model catalog refresh, version check or telemetry: the only egress is
# pat-service.
ENV PI_OFFLINE=1 PI_SKIP_VERSION_CHECK=1 PI_TELEMETRY=0
USER 10000

FROM base AS opencode
ARG OPENCODE_VERSION=1.18.32
RUN npm install -g --no-audit --no-fund opencode-ai@${OPENCODE_VERSION} \
 && npm cache clean --force \
 && opencode --version
COPY opencode.mjs ./
USER 10000

# DeepSeek Harness: `dsh --profile acp` for the chats and `dsh web` for the
# user's browser UI (dsh.mjs). DSH_PERMISSION_MODE: the pod is the sandbox
# (gVisor, NetworkPolicy), so dsh runs without its own bwrap sandbox and
# approval prompts; no telemetry.
FROM base AS dsh
ARG DSH_VERSION=0.2.0-rc.2
RUN npm install -g --no-audit --no-fund @deepseek-ai/dsh@${DSH_VERSION} \
 && npm cache clean --force \
 && dsh --version
COPY dsh.mjs ./
ENV DSH_PERMISSION_MODE=danger-full-access DSH_TELEMETRY_MODE=DISABLED
USER 10000

# Hermes over ACP (docs/adr/0021): the upstream image, whose [all] extra
# includes `hermes acp` and which ships Node 26, plus agent-adapter. Keep the
# digest in step with the upstream release the catalog is tested against;
# without AGENT_PROTOCOL=acp the broker runs `hermes gateway run` from it
# unchanged. The broker sets the command and uid.
FROM nousresearch/hermes-agent:v2026.9.14@sha256:99641e57ec762c59e54cb44aa6746b7fc68c18b3c5ddb088af54234c613d9294 AS hermes
COPY package.json server.mjs acp.mjs /opt/agent/
