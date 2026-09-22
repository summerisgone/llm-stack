# Open WebUI v0.11.3 REST reference

Every endpoint below was exercised against a live v0.11.3 instance. Auth is
`Authorization: Bearer $OPENWEBUI_API_KEY` unless noted.

## Contents

- [Discovery](#discovery)
- [Tools](#tools)
- [Knowledge and files](#knowledge-and-files)
- [Automations](#automations)
- [Models and chats](#models-and-chats)
- [Inference](#inference)
- [Response-shape quirks](#response-shape-quirks)
- [Admin-only surfaces](#admin-only-surfaces)

## Discovery

| Method | Path | Notes |
|---|---|---|
| GET | `/api/config` | Unauthenticated. `version`, `features`. Feature keys appear only when enabled |
| GET | `/api/v1/auths/` | Your user: `id`, `role`, `email` |
| GET | `/api/v1/auths/api_key` | Echoes your key - confirms the key is live |
| GET | `/api/models` | Models visible to you, with `urlIdx` (connection index) and `info.base_model_id` |

Probing for an endpoint: an unknown path returns the SPA as HTML with **200**. Check the body.

## Tools

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/tools/` | List. `content` is always `null` here |
| GET | `/api/v1/tools/export` | All tools **with source** - this is how you read code |
| GET | `/api/v1/tools/id/{id}` | One tool, metadata and specs |
| POST | `/api/v1/tools/id/{id}/update` | Body: `{id, name, meta, content}`. `content` is the module source |
| GET | `/api/v1/tools/id/{id}/valves` | Current values. Only shows keys that were explicitly stored |
| POST | `/api/v1/tools/id/{id}/valves/update` | **Replaces** the set. Send every field you want kept |

`meta.manifest` mirrors the module docstring (`title`, `description`, `version`, `requirements`).
Declared `requirements` are installed at startup by `install_tool_and_function_dependencies()`.

## Knowledge and files

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/knowledge/` | `{"items":[{id, name, file_count, write_access}], "total"}` |
| GET | `/api/v1/knowledge/{id}` | Detail. `files` may be `null` for non-admins even when populated |
| POST | `/api/v1/knowledge/create` | New collection |
| POST | `/api/v1/knowledge/{id}/file/add` | `{"file_id": "..."}`. 400 if the file is still processing |
| POST | `/api/v1/knowledge/{id}/file/remove` | `{"file_id": "..."}` |
| POST | `/api/v1/files/` | multipart `file=@path;type=text/markdown` -> `{id, ...}` |
| GET | `/api/v1/files/` | `{"items":[...], "total": n}` - a dict, not a list |
| GET | `/api/v1/files/{id}/process/status` | `pending` / `completed` / `failed` |
| GET | `/api/v1/files/{id}/data/content` | `{"content": "..."}` |

Ingestion order is fixed: upload, poll to `completed`, then add to the collection.

```bash
FID=$(curl -s -X POST "$BASE/api/v1/files/" -H "Authorization: Bearer $KEY" \
        -F "file=@doc.md;type=text/markdown" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
until [ "$(curl -s -H "Authorization: Bearer $KEY" "$BASE/api/v1/files/$FID/process/status" \
        | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')" = completed ]; do sleep 2; done
curl -s -X POST "$BASE/api/v1/knowledge/$KID/file/add" -H "Authorization: Bearer $KEY" \
     -H "Content-Type: application/json" -d "{\"file_id\":\"$FID\"}"
```

## Automations

Router: `/api/v1/automations`. All endpoints take `get_verified_user`, then
`check_automations_permission`, which returns **403** with body `{"detail":"401 Unauthorized"}` when
`automations.enable` is off or the role lacks `features.automations`. Both are admin settings.

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/automations/list` | Paginated. Note: `/api/v1/automations/` is the SPA |
| POST | `/api/v1/automations/create` | See body below |
| GET | `/api/v1/automations/{id}` | Includes `last_run_at`, `next_run_at` (**nanoseconds**) |
| POST | `/api/v1/automations/{id}/update` | Same body as create |
| POST | `/api/v1/automations/{id}/toggle` | Enable / disable |
| POST | `/api/v1/automations/{id}/run` | Fire immediately - the dev loop |
| GET | `/api/v1/automations/{id}/runs` | `[{id, chat_id, status, error, created_at}]`, newest first |
| DELETE | `/api/v1/automations/{id}/delete` | Removes the automation and its runs |

```json
{
  "name": "Morning digest",
  "is_active": true,
  "data": {
    "model_id": "my-model-preset",
    "prompt": "...",
    "rrule": "DTSTART;TZID=Asia/Yekaterinburg:20260921T070000\nRRULE:FREQ=DAILY"
  }
}
```

`AutomationData` is `{prompt, model_id, rrule, terminal?, target?}`. Tools and knowledge are **not**
fields here - they come from the model preset. `target` selects a channel; omit it to produce a chat.

RRULE validation: `COUNT=` requires a `DTSTART`. Sub-daily frequencies snap to clock boundaries.

`status: success` only means the pipeline ran to completion. It says nothing about the answer.

## Models and chats

| Method | Path | Notes |
|---|---|---|
| GET | `/api/v1/models/model?id={id}` | Preset detail: `base_model_id`, `meta.toolIds`, `meta.knowledge`, `params.system` |
| GET | `/api/v1/chats/?page=1` | Chat list |
| GET | `/api/v1/chats/{id}` | Full chat |

Messages live at `chat.history.messages` (a **dict** keyed by message id). `chat.messages` may be empty.
Per assistant message: `content`, `done`, `statusHistory` (tool and RAG progress), `error`, `sources`.

```bash
curl -s -H "Authorization: Bearer $KEY" "$BASE/api/v1/chats/$CID" | python3 -c "
import json,sys
for m in json.load(sys.stdin)['chat']['history']['messages'].values():
    if m.get('role') == 'assistant':
        print('done:', m.get('done'))
        print([s.get('description') for s in (m.get('statusHistory') or []) if s.get('description')])
        print(m.get('content') or '<EMPTY>')"
```

## Inference

`POST /api/chat/completions` - OpenAI-shaped, plus Open WebUI extras:

- `tool_ids: ["..."]` - offers the tools. With `stream:false` the response comes back with
  `finish_reason: "tool_calls"` and the call **unexecuted**. Tool execution happens in the chat pipeline.
- A model preset's own tools and knowledge are not attached automatically on this endpoint; pass
  `tool_ids` explicitly.

Whether this works headlessly at all depends on the connection's `auth_type` - see SKILL.md.

## Response-shape quirks

- `/api/v1/files/` returns a dict `{items, total}`; several sibling endpoints return bare lists.
- `next_run_at` / `created_at` on automations are nanosecond epochs. `datetime.fromtimestamp(v/1e9)`.
- Unknown paths return the SPA with 200.
- `{"detail":"401 Unauthorized"}` is a permission gate returning HTTP 403, not an auth failure.

## Admin-only surfaces

These need an admin role; with a plain user key you get HTML or a permission error:

- `/api/v1/openai/config` - connections, including each one's `auth_type`
- `/api/v1/functions/` - Functions (filters/pipes) are admin-only to list and create
- `/api/v1/users/default/permissions`
- Admin Settings - enabling automations, adding connections, per-role feature permissions

When you need to know a connection's `auth_type` and lack admin, infer it from the upstream error string
(see the table in SKILL.md) or ask the operator.
