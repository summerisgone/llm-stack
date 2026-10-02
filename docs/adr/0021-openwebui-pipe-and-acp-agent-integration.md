# 0021: Open WebUI Pipe for agent interaction, with ACP behind agent-broker

## Status

Proposed on 2026-10-03. Based on repository code and upstream documentation
read on that date, without a runtime compatibility test.

Implemented for dsh on 2026-10-03, not yet deployed; gates 1-6 below are
open. The Pipe is `config/openwebui/agent_pipe.py`; the broker protocol is
NDJSON per turn plus a permission POST (`agent-broker/cmd/agent-broker/
interact.go`), with no reconnect: a closed stream cancels the turn. Edit,
regenerate and branch are rejected. Runbook:
[operations/agents.md](../operations/agents.md#interactive-agent-pipe-dsh).
Pipe hooks were read in the v0.11.4 source (`functions.py`: `__oauth_token__`,
`__event_call__`), not yet exercised in a browser.

Extends [ADR 0014](0014-hermes-curated-catalog-and-worker-slots.md) with an
agent interaction path that can display tool progress and ask for permission.
Replaces its OpenAI-only agent contract for runtimes migrated under this ADR;
its identity, profiles, catalog and worker-slot decisions remain applicable.
Extends [ADR 0020](0020-dsh-runtime-and-per-user-web-ui.md): dsh is the first
ACP runtime used to validate the new path. Its own web UI remains separate.
For the new Pipe path, interactive permission handling replaces 0020's
automatic permission approval where the runtime supports requests.

## Context

Open WebUI currently reaches agent-broker through an OpenAI-compatible
connection authenticated with the user's Keycloak token. The broker verifies
identity and roles, handles onboarding and `/skills`, acquires a worker slot,
and proxies chat completions to the user's runtime pod.

Hermes serves that API natively. `agent-adapter` fronts pi through its SDK,
OpenCode through its HTTP API, and dsh through ACP over stdio. The adapter
forwards text and reasoning, but has no user-facing path for structured tool
events or permission requests. It passes only the last user message to a
persistent session selected by the Open WebUI chat ID. Edit, regenerate and
branch semantics are consequently not equivalent to replaying chat history.

ACP here means **Agent Client Protocol**: JSON-RPC with initialization,
capability negotiation, sessions, prompts, updates and cancellation. It does
not replace the OpenAI-compatible model inference API or MCP tool protocol.

The [openclaw-openwebui-pipe reference](https://github.com/D-jai/openclaw-openwebui-pipe)
demonstrates connecting an agent through an Open WebUI Function, displaying
streamed text and statuses without forking Open WebUI. Although its README
calls the transport ACP, its code speaks OpenClaw's Gateway protocol:
`connect.challenge`, `connect`, `chat.send`, and `req/res/event` frames.
It is not an Agent Client Protocol client. Its shared credentials and fixed
`agent:main:main` session are also unsuitable for our per-user fleet.

Open WebUI Pipes provide model entries, streaming, UI events and a browser
request/response hook. These are a plausible integration surface; their
availability and behavior must be checked against the Open WebUI version in
`versions.lock.env`. ACP's standard stdio transport fits a subprocess inside
an agent pod. Remote HTTP/WS support is still evolving and is not assumed to
provide a ready-made interoperable Kubernetes endpoint.

## Decision

### 1. Use a repository-owned Pipe, without an Open WebUI fork

Implement a small async Open WebUI Pipe for the personal agent models. The
Pipe owns presentation and translates broker events into text, reasoning,
status, tool-result and permission UI. Take the extension pattern from the
reference project, not its protocol, device credentials or shared session.

Keep direct model chat on the existing inference connection. Roll out the
Pipe per runtime; retain the existing agent connection as a rollback path
until acceptance gates pass. Existing chat history must remain accessible.

### 2. Keep agent-broker as the identity and lifecycle boundary

The target path is:

```text
Open WebUI Pipe
  -> authenticated broker interaction endpoint
     Keycloak subject and role, runtime, chat/session ownership,
     onboarding, /skills, catalog notices, slots and idle eviction
  -> authenticated ACP bridge inside the user's runtime pod
  -> agent subprocess over ACP stdio

Agent -> pat-service /v1 and /mcp/<name>/ with the user's agent PAT
```

The Pipe forwards the user's Keycloak access token using a supported WebUI
hook. The broker derives identity only from a validated token, never from
`__user__`, a supplied user ID or a session ID. No shared administrator key
substitutes for user authentication. If the pinned WebUI cannot supply the
token, migration is blocked until a supported authenticated path is proven.

The broker remains the only caller admitted to the pod's interaction API.
Do not expose agent pods publicly or add Kubernetes exec privileges to WebUI
to reach stdio. Keep per-pod authentication, gVisor, NetworkPolicy, per-user
PATs and catalog enforcement. ACP clients may not inject arbitrary MCP
servers, provider settings or paths that bypass the curated profile.

### 3. Separate presentation, network transport and ACP execution

The Pipe does not launch agent processes. The pod bridge negotiates ACP
capabilities, manages the subprocess connection and exposes authenticated
interaction to the broker. Reuse `agent-adapter` for this responsibility.

Use a bidirectional event path for prompts, updates, cancellation and
permission responses. Select and document the network transport and its
reconnect behavior during the dsh prototype. Either a documented broker
protocol or a pinned remote ACP transport is acceptable; neither may be
advertised as standard ACP without meeting its lifecycle and framing rules.

Do not move the broker's compute backend into the Pipe or create a second
owner for pods, profiles, routes or catalog configuration.

### 4. Make sessions and turns explicit

Scope session mappings by validated Keycloak subject, runtime and WebUI chat
branch. Persist mappings on the user's profile so eviction or pod restart
does not silently create a different conversation. Check ownership on every
session, event subscription, cancellation and permission response.

Correlate events by session and turn; serialize prompts within one session.
Retries must not execute a prompt or tool action twice. A disconnected
stream must not imply that an unfinished operation succeeded.

Before enabling the Pipe, define and test edit, regenerate and branch
behavior. Use capability-supported load/fork operations where available;
otherwise create an explicit new session from supported context or reject
the operation with a clear message. Never append an edited historical turn
silently to the existing agent session. Existing sessions are migrated only
where the runtime supports it; otherwise retain the legacy path.

### 5. Render tool activity and round-trip permission requests

Map ACP tool updates to display-only WebUI events or content blocks. Do not
emit executable OpenAI `delta.tool_calls` for tools the agent already runs:
WebUI must not execute them again. Render reasoning only when provided and
supported, and report failures as failures rather than successful completion.

Use WebUI's browser response hook for ACP permission requests where supported.
Bind each response to its pending request, user, session and turn. Denial,
timeout, cancellation or a missing browser response must not become approval.
Do not auto-approve merely because the agent runs under gVisor. The bridge
must handle cancellation while a permission request is pending.

A permission UI controls requests actually emitted by the runtime; it does
not add a security boundary to a runtime that executes tools without asking.
Validate dsh's permission mode before promising approvals. File and terminal
capabilities remain within the user's pod and workspace. Handle unsupported
attachments explicitly rather than silently discarding them.

### 6. Migrate by capability, starting with dsh

| Runtime | Starting point | Migration requirement |
| --- | --- | --- |
| dsh | Existing ACP stdio client in `agent-adapter/dsh.mjs` | Validate events, permissions, cancellation and persistent sessions first |
| OpenCode | Existing HTTP client; upstream offers `opencode acp` | Verify the pinned release and preserve provider, MCP and offline settings |
| Hermes | Native chat-completions server; upstream documents ACP | Verify ACP availability and dependencies in the pinned image, profile loading and session persistence |
| pi | In-process SDK; third-party `pi-acp` exists | Evaluate compatibility with the pinned SDK and MCP extension; retain the SDK path until a supported bridge passes |

No all-runtime cutover is required. Package the Pipe and its dependencies
from reviewed, pinned sources for air-gap installation. No runtime `npx`,
pip downloads or registry updates are allowed. Installation and upgrades
must be reproducible from the repository rather than manual UI-only edits.

## Alternatives

- **Keep only chat completions.** Lowest maintenance, but does not provide
  structured agent interaction or a permission response path.
- **Unify ACP inside pods only.** Useful for adapter reuse, but leaves richer
  UI events unexposed; it can be an intermediate implementation step.
- **Install the OpenClaw Pipe unchanged.** Rejected: different wire protocol,
  shared session and credentials, no fleet lifecycle integration.
- **Fork Open WebUI or replace it with a new client.** Deferred. First prove
  the supported Pipe and event hooks; a frontend fork needs a separate
  decision if those hooks cannot meet the required interaction behavior.

## Consequences

- Agent interaction gains structured progress and, where supported,
  permission prompts while using the existing login, profiles and capacity.
- The platform maintains a Pipe, event contract and ACP capability handling
  in addition to runtime-specific configuration.
- ACP does not make memory, tool policy, session storage or feature support
  identical across runtimes. Unsupported features remain explicit.
- Active turns, including pending permission requests, hold a worker slot;
  an idle UI connection alone must not prevent eviction. Bound abandoned
  requests so they cannot reserve capacity indefinitely.
- Existing inference routing, fair share, PAT accounting and the dsh web
  origin remain governed by their existing ADRs.

## Verification and implementation order

Implementation is separate from this document. Each failed gate blocks
rollout of the affected runtime.

1. **WebUI spike on the pinned version:** prove model entries, OAuth token
   forwarding, text/reasoning/tool display, browser confirmation, errors and
   Stop without a frontend fork. API calls without browser hooks fail or
   deny interactive permission requests explicitly.
2. **dsh end to end:** stream tool activity, approve and deny an actual
   permission request, cancel during execution and during a pending request,
   and preserve the curated provider/MCP configuration.
3. **Isolation and concurrency:** two users and two chats per user; no
   cross-user session access or cross-turn event leakage. Invalid tokens,
   missing roles, forged identity and stale permission replies are rejected.
4. **History and recovery:** verify edit/regenerate/branch behavior, repeated
   submissions, pod eviction, broker restart, reconnect and runtime death.
   Preserve durable mappings and report unrecoverable turns explicitly.
5. **Fleet behavior:** onboarding, `/skills`, catalog updates, `K+1` pressure,
   active-turn protection and idle eviction still obey ADR 0014. Stop and
   abandoned confirmations release capacity after bounded cleanup.
6. **Packaging and rollout:** install from an offline bundle; validate
   manifests with `make verify` when changed, run `make agents-test` and
   extend the agent smoke coverage. Document per-runtime enable/rollback
   and update the English/Russian handbook only when behavior is implemented.
7. **Other runtimes:** validate OpenCode, Hermes and pi individually against
   their pinned artifacts before enabling their Pipe entries.

Out of scope: implementing this ADR now, replacing model inference or MCP,
installing OpenClaw, background/scheduled agents, replacing the dsh web UI,
and exposing a public general-purpose ACP service for external IDE clients.

## References

- [Current broker contract](../../agent-broker/cmd/agent-broker/backend.go)
- [Current adapter](../../agent-adapter/server.mjs) and [dsh ACP client](../../agent-adapter/dsh.mjs)
- [Open WebUI Pipe Functions](https://docs.openwebui.com/features/extensibility/plugin/functions/pipe/)
- [Agent Client Protocol](https://agentclientprotocol.com/protocol/v1/overview) and [transports](https://agentclientprotocol.com/protocol/v1/transports)
- [OpenClaw Pipe source](https://github.com/D-jai/openclaw-openwebui-pipe/blob/main/openclaw_pipe.py) and [Gateway protocol](https://docs.openclaw.ai/gateway/protocol/transport)
- [OpenCode ACP](https://dev.opencode.ai/docs/acp/), [Hermes ACP](https://hermes-agent.nousresearch.com/docs/user-guide/features/acp/), [pi-acp](https://github.com/svkozak/pi-acp)
