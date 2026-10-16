# 0025: Detect the agent harness of each inference request

## Status

Accepted.

## Context

Sessions in Langfuse and in the pat-service statistics are opaque
`sess-<hex>` ids (ADR 0011). Traffic comes from several agent harnesses -
Hermes, opencode, Qwen Code, pi, DeepSeek Harness, Kilo - and each harness
also makes auxiliary calls of its own (titles, context compaction, memory
extraction, safety classifiers). None of that is visible.

A 21-day sample of Langfuse generations (first request of ~2200 sessions)
showed:

- The request body is the only usable signal: `User-Agent` is not recorded
  anywhere, and no harness sends an identifying header.
- Every harness has a stable system-prompt opening ("You are Hermes Agent",
  "You are opencode", "You are Qwen Code", "operating inside pi",
  "powered by DeepSeek Harness", ...).
- About 60% of sessions are auxiliary calls with their own fixed prompts; a
  few (Hermes compaction, Qwen Code's message router) put the instructions in
  a user message, with no system message.

## Decision

1. `pat-service/internal/harness` classifies each `/v1/chat/completions`
   body into a harness and a role by an ordered rule table: substring of the
   first system message (first message when there is none, first 2 KiB),
   optionally constrained by tool names; then tool-name-only rules; then
   `User-Agent`; else `unknown/unknown`.
2. Detection is per request, with no stored state. Main-session requests
   resend the system prompt every step; auxiliary calls are fresh-context and
   already get a session of their own, so a session's value is the
   aggregate of its rows.
3. pat-service sets `X-Agent-Harness: <harness>/<role>` upstream and strips a
   client-supplied one (same rule as `X-Session-Key`, ADR 0011). The AI
   Gateway maps it to Langfuse metadata `agent_harness`.
4. The ledger gets `qos_events.agent_harness` and `agent_role`. The admin
   user page (filter, sessions and requests) and the `/platform` session
   table show them.

## Consequences

- Rows written before migration 0004 stay empty: the ledger keeps no prompt
  content, so they cannot be classified later.
- Prompt signatures drift with harness releases; an unmatched harness shows
  as `unknown`, never breaks a request. Adding one is one rule plus a test
  case.
- Bodies over `QOS_FINGERPRINT_MAX_BODY_BYTES` get no harness, same as no
  session.
- Prompts seen in the sample but not attributable with confidence ("security
  reviewer ... shell commands", "context summarization agent") stay
  `unknown`.
