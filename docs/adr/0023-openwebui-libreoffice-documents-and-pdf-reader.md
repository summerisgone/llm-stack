# 0023: Co-editing in Notes, revision history and LibreOffice export

## Status

Proposed on 2026-10-06. Implementation is a separate change: Notes revision
history, the LibreOffice service and the adapters are not implemented or
deployed yet. Built-in Notes exists in the pinned version; collaboration
with our model and personal agents has not yet been tested. This ADR is
accompanied by an executable test of Open WebUI's existing PDF reader.

Extends [ADR 0017](0017-web-search-mcp-openserp-kagent.md) and
[ADR 0018](0018-repowise-codebase-intelligence.md): tools are reached through
pat-service, and one Helm template owns tool-server configuration.
[ADR 0021](0021-openwebui-pipe-and-acp-agent-integration.md) remains the
agent interaction decision; document operations do not require switching
ordinary model chat to an agent model.

## Context and scope

The primary workflow is a user and a model working on the same text in the
Notes editor and its attached chat. The user edits directly, selects
passages and asks the model for changes. Every save has a revision, and a
selected revision can be exported to DOCX/ODT/PDF through LibreOffice.
Headings, paragraphs, lists and simple tables are sufficient for the first
release. Notes holds the source text; office files are exports, not parallel
editable originals.

A separate, later workflow imports and structurally edits existing DOCX/ODT
files, preserving their office structure in the document service. Converting
such a file to Notes creates a new text document and may lose formatting;
reverse synchronization is not promised. Calc XLSX/ODS and Impress PPTX/ODP
are separate stages. Collabora/WOPI is needed if users must edit office
layout directly in the browser.

At the time of this decision, the repository pins Open WebUI **v0.11.4** in
`versions.lock.env`. Its workload lives in `k8s/base/applications.yaml`.
Native MCP connections with `auth_type: system_oauth`, a pat-service server
registry and a shared agent MCP catalog already exist. There is no document
service, LibreOffice, pinned external PDF extractor or configured connection
from WebUI RAG to bge-m3. Admin settings do not survive restarts with
`ENABLE_PERSISTENT_CONFIG=false`.

Version v0.11.4 includes a Notes API and `NoteEditor`: the note's attached
chat directs the model to use `view_note` and `replace_note_content`. The
built-in editor is a starting point, but its Undo/Redo is not durable server
history. Verify the documented range-edit mode with `expected` against the
pinned image; it does not replace an atomic revision check. These built-in
WebUI tools do not automatically become tools for Hermes/Pi/OpenCode behind
agent-broker.

The pinned source routes an ordinary PDF to `PDFLoader` and also supports
external extractors. The Files API stores the original file, extracted
`data.content` and processing status separately. An HTTP 200 upload response
does not prove that the PDF was read. An LLM answer is not proof either:
the model may answer from general knowledge.

Separate the responsibilities:

| Task | Responsible component |
| --- | --- |
| User and model co-edit text | Notes and one server write path with revision history |
| Export a saved Notes revision | Document service and isolated LibreOffice worker |
| Read PDFs and provide text for chat/RAG | Open WebUI extractor, with separate OCR validation |
| Structurally edit imported office files | Later document-service stage using LibreOffice |
| Edit office layout in the browser | Possible future Collabora Online/WOPI integration, outside the first stage |

## Decision

### 1. Use Notes as the editor and text source

Use the existing Notes editor and attached chat. Ordinary models use the
built-in note tools; MCP provides export and access for external personal
agents. Including a note in RAG context or attaching it to a chat does not
itself grant permission to change it. On the pinned WebUI version, verify
Notes access for a regular user, native tool calling and editor updates
after both manual and model edits.

```text
Notes editor + attached chat / built-in note tools
                  |
          one Notes write API
          ACL + base_revision + save
                  |
       current note + immutable revisions
                  |
       snapshot of a selected saved revision
                  |
         documents API -> queue -> LibreOffice -> DOCX/ODT/PDF

Personal agent -> pat-service /mcp/documents/
               -> Notes adapter -> the same Notes write API
```

**Durable history.** Implement a server-side Notes extension with a revision
table and current `revision_id` in the same database as the note. One
transaction checks `base_revision`, inserts an immutable snapshot and
updates the current pointer/content. Revision fields include note ID,
revision ID, parent revision, author and initiator (human/model/agent),
timestamp, idempotency key, hash, full text/editor structure and its schema
version. The snapshot format must restore formatting, not just plain text.

The current WebUI database is SQLite on a PVC; do not create a second
canonical Notes history in the document service's PostgreSQL database.
Schema migration, backups and restore remain WebUI responsibilities. Create
an initial revision for each existing note during migration; do not invent
past history. Show unsaved local input separately. A revision is created on
a successful save/autosave, not for every keystroke.

Every write path, including manual save/autosave, built-in tools, the agent
adapter and restore, must call the same compare-and-swap operation. A
separate observer, polling or recording history after saving cannot provide
this guarantee: it can miss a change or record different text. If plugin
hooks do not cover every path, a small maintained backend/frontend patch to
the pinned WebUI or an upstream extension is required. Establishing this is
a mandatory technical spike; do not promise a Tool/Pipe-only implementation
without changes to WebUI.

On conflict, the server returns 409. Keep the user's draft in the editor and
offer a comparison with the current revision. Do not overwrite it with a
model event or an automatic save retry. An `expected` check protects the
selected passage in addition to `base_revision`. The MVP does not
automatically merge simultaneous edits to the same passage.

**Model edits.** By default, the model creates a proposal containing the
base revision, target ranges/blocks, expected old text and replacement
content. The UI displays a diff and Apply/Reject controls. Only an
authenticated user action applies the proposal; a tool argument such as
`approved=true` is not consent. Proposals are bound to the owner, note and
revision, and application is idempotent. Clicking again after acceptance or
rejection does not create another revision. A stale proposal requires a
fresh read and diff; a late approval does not authorize an overwrite. Route
the built-in `replace_note_content` through this mechanism too, or it will
bypass validation and history.

History remains available after closing the browser or restarting WebUI.
Restore creates a new revision from an old snapshot while checking the
current head; it never deletes or rewrites previous revisions. Check access
to old revisions against the note's current permissions. Quotas must include
history; exhaustion produces an explicit save error rather than silently
deleting history. Retention and user deletion cover revisions and exports.

**Personal agents.** Add `note_read`, `note_propose_edit`, `note_versions`
and `note_export` to `documents-mcp`. They use the same Notes API and user
permissions; other users must not receive a complete inventory of notes.
Edit approval remains a UI action. Agent-broker gets no direct access to the
WebUI database and does not keep a copy of the note as the head. The MCP
adapter requires a verified delegation channel: the Notes bridge validates
a short-lived signed assertion from the trusted gateway, scoped by audience,
subject and operation. Map Keycloak sub to WebUI user ID on the server using
a trusted OAuth association; reject the request if none exists. A name,
email or agent-supplied user ID is insufficient. This channel still needs
implementation and testing; a shared admin key is prohibited.

### 2. Use MCP for operations and isolated LibreOffice for execution

Add a repository-owned `documents-mcp` using **MCP Streamable HTTP** behind
`pat-service /mcp/documents/`. Run headless LibreOffice in a separate CPU
worker, outside the Open WebUI process and GPU inference workers. Native
tool calling by the selected model must pass an acceptance test.

```text
Ordinary Open WebUI chat
  |-- native MCP + user's Keycloak token
  |     -> pat-service /mcp/documents/ -> documents API -> queue -> worker
  |                                                              -> LibreOffice
  |-- WebUI file adapter -> pat-service, internal transfer API
  |                          -> the same ACLs and document storage
  \-- Files API -> extractor -> text/context/RAG

Personal agent -> pat-service /mcp/documents/ with a personal agent PAT

Notes: source text and its revisions in the WebUI database
Document service: export jobs and imported office-file versions;
                  metadata in PostgreSQL, objects in MinIO
File adapter: result -> WebUI Files API -> attachment/link in chat
```

No `mcpo` is needed: the selected server supports the native MCP transport
directly. OpenAPI remains a viable transport alternative, but would require
an additional client path for agents. A shared MCP server uses the existing
authentication, metrics and catalog.

A Notes export request contains `note_id`, a saved `revision_id`, the format
and an approved template version. The adapter reads that exact snapshot,
checks ACLs and passes it to the worker. Later note edits do not change the
input of an export already in progress. Save an unsaved draft first; a save
conflict prevents export from starting. The result key includes the snapshot
hash, format, template and renderer/font versions, not just the note ID.

The worker deterministically converts supported Notes blocks into a document
using the template, then LibreOffice saves DOCX/ODT/PDF. An implementation
spike selects and pins the converter from Notes structure to ODT/DOCX;
LibreOffice alone does not implement this step. Pass attachments by approved
IDs after checking access; do not execute arbitrary HTML, external URLs or
embedded scripts. Unsupported blocks cause an explicit error or an agreed
formatting-loss report, never silent omission. Store the note/revision ID,
hash and export parameters with the result; check current permissions on
download. Editing an exported DOCX externally does not update Notes: only
an explicit import into a new document is supported.

### 3. Make file transfer explicit rather than assuming MCP provides it

A native MCP connection does not guarantee that the server can access
attachment bytes or register a result in Open WebUI Files. A UUID in a tool
argument, a model-supplied URL and a filename are not evidence of access.

Provide a small repository-owned **Open WebUI import/export Tool**. It runs
in the authenticated WebUI context and uses WebUI's Files access checks.
Document operations stay in MCP. The Tool does not launch LibreOffice, read
SQLite directly, mount the WebUI PVC in another service or use a shared
admin API key.

Import sequence:

1. The Tool receives a reference to a specific attachment in the current
   request. It checks the user's access through server-side WebUI Files
   helpers/API. It accepts no arbitrary model-supplied path, URL or `user_id`.
2. It sends bytes through an internal `pat-service /documents/` transfer API
   with the user's Keycloak token. This API and its routes still need to be
   implemented. pat-service validates the token and derives `sub` itself.
3. The document service stores the original, hash, MIME type, size and owner,
   and returns opaque `document_id` and `version_id` values for MCP operations.

On export, the Tool downloads an authorized artifact through the same
gateway, registers it in Files as the current WebUI user and returns a
normal file/attachment link. The browser downloads it through WebUI with
normal authentication. No tokens in Markdown URLs, public MinIO bucket or
shared directory of downloadable files.

The first technical spike must verify attachment, OAuth and file-delivery
Tool hooks on the pinned version, including session expiry. MCP documentation
alone does **not** establish that this path is available. If standard hooks
cannot support it safely, block rollout and revisit the decision in a new
ADR. Do not substitute a shared administrator account for personal
authorization. The Pipe from ADR 0021 may later be extended for the agent UI,
but does not replace this ordinary-chat test.

### 4. Constrain operations and create versions instead of overwriting

The first stage implements the Notes contract in section 1 and export jobs.
The following operations belong to the later office-document stage, where
the document service owns the original and its versions:

| Tool | Office-stage contract |
| --- | --- |
| `document_list` / `document_get` | Only the current owner's documents, metadata and bounded text |
| `document_create` | A document from typed blocks and an approved template |
| `document_edit` | An allowed list of structural edits, `base_version`, `idempotency_key` |
| `document_export` | A specific version in an allowed format: DOCX, ODT or PDF |
| `document_job_get` / `document_job_cancel` | Status, error and cancellation for a job owned by the user |

The Writer MVP supports paragraphs, headings, text replacements with an
expected match count and table cells. Zero or ambiguous matches produce an
explicit error rather than an arbitrary edit. CLI `--convert-to` is enough
for export; structural edits require the UNO API. Do not expose `run_python`,
shell, arbitrary UNO calls, macro execution or server filesystem access to
the model.

An edit creates an immutable new version with a parent and a brief change
description. Optimistic `base_version` checks prevent lost concurrent edits.
Bind the idempotency key to the owner, operation and input-argument hash;
reusing it with different arguments produces a conflict. A retry after a
network failure returns the same job/version.

A long-running operation immediately returns `job_id`, followed by bounded
client polling. States are queued/running/succeeded/failed/cancelled. Success
means the output exists, is nonempty, passes format validation and has been
atomically registered. LibreOffice's exit code alone is insufficient. If a
worker dies, either retry safely from the original version or mark the job
failed; never publish a partial result.

### 5. Identity, storage and isolation

Check permissions on **every** document/version/job/artifact, including
download, polling and cancellation. The owner is the Keycloak `sub` validated
by pat-service. Do not assume WebUI user ID equals Keycloak `sub`; the adapter
associates them only through trusted request context. The server does not
trust client-supplied `X-User-*`, `chat_id` or MCP session ID. Bind MCP sessions
to their owners too.

Enabling tools in a chat grants access to the tool, not to other users'
documents. The MVP has no shared library, publishing, original-file deletion,
existing-version overwrite or ACL bypass for `ai-admin`. Document contents
are input data, not instructions for tools.

Each document kind has one canonical storage owner. For `source_kind=note`,
that is Notes with revisions in the WebUI database; the document service
keeps only immutable export-input snapshots and derived files. For
`source_kind=office`, it is the document service: PostgreSQL stores ACLs,
versions and jobs; MinIO stores immutable originals/results. WebUI Files
stores incoming attachments and copies delivered to users, not a second
editable version database. Keep a reference to the canonical version in
copy metadata. Deleting a chat does not delete the canonical document;
working-file TTLs and user-data deletion procedures must cover both stores
and backups. Do not allow cross-user deduplication that reveals whether a
file exists.

Each worker receives one input snapshot, a separate working directory and
a LibreOffice profile (`-env:UserInstallation=file:///...`). UNO is available
only to the local worker process through a pipe/loopback connection, without
a Kubernetes Service. Run unprivileged, without a service-account token or
host mounts, with a read-only root filesystem, seccomp and bounded scratch
storage. Do not treat `--headless` as a sandbox.

Disable macros and external-link updates through the profile and UNO load
properties; the worker has no network access. A separate control component
retrieves and delivers files. Validate MIME type, size, page count, unpacked
archive size, timeout, memory and process count. Explicitly reject
password-protected files in the MVP. A malicious or hung file fails its job,
not all of WebUI. Test gVisor compatibility if used for the worker rather
than assuming it.

Initial limits for measurement: 20 MiB input, 100 rendered pages, 120 seconds
per job, one active job per user, two worker slots and 20 queued jobs per
service. Complex documents may exceed these limits; errors must identify
the specific limit. This is a separate CPU queue that neither occupies the
GPU nor changes EPP bands. pat-service rate limiting does not replace queue
and data-size limits; admission must fail closed when capacity accounting
is unavailable, even if the MCP rate limiter fails open.

### 6. PDF reading is a separate, tested path

Keep Open WebUI's existing text-PDF reader for the first stage. LibreOffice
is used to **create PDFs from office documents**; importing arbitrary PDFs
into Draw is not the selected reading or OCR path. Do not promise that a
PDF can become an editable Word document without structural loss.

For scans, the next stage is a local extractor/OCR engine. First measure the
current PDFLoader with OCR; if it fails the corpus, compare local Docling
and Tika with OCR. Pin the choice, image, models, RU/EN language data and
resource requirements based on test results. Do not use cloud recognition
or download models while processing. Enabling an extractor does not
configure embeddings: WebUI's bge-m3 connection and RAG behavior require
separate validation.

User-visible reading outcomes must distinguish extracted text, OCR required,
unsupported format/password and processing failure. Empty text must not
appear as successful reading. For cited answers, verify that page numbers
and the source file version are preserved. Extracting table numbers does
not prove correct row/column reconstruction or accurate financial calculations.

### 7. Object ownership, packaging and operations

- Workloads, Services, storage and NetworkPolicies belong to `k8s/base`;
  the remote chart receives them only through `scripts/helm-render`.
  Profile-specific changes belong in the overlay. Do not manually duplicate
  a document Deployment in `helm/airgap-stack/templates`.
- Extend the MCP registry in pat-service. Give the new internal transfer API
  the same token validation and separate size limits. NetworkPolicy admits
  only pat-service to the API. Add no public routes for `/mcp/`,
  `/documents/`, UNO or MinIO.
- Add the tool connection to the existing `openwebuiToolServers`; the current
  Helm template remains the ConfigMap owner. Extend the agent catalog in
  `config/agents/base-profile/mcp-servers.yaml`.
- `DOCUMENTS_ENABLED=false` by default controls server availability,
  Tool/catalog exposure and worker startup. Disabling it neither deletes
  documents nor disables Notes save history. Introduce the versioned Notes
  write path through a separate migration and WebUI rollout; rolling back
  tools must not restore writes that bypass history. Verify tool-config
  delivery separately for local-mac, since it is currently tied to the
  remote overlay. Manifest changes require `make verify`.
- Keep the file Tool source in `config/openwebui` and install it with an
  idempotent script. The connection must not exist only in the admin UI.
- Keep the Notes extension, migrations and pinned WebUI patch in the
  repository too; the derived image build and version must be reproducible.
  On every WebUI upgrade, verify all write paths and editor-snapshot schema
  compatibility. Do not manually edit code in a running container.
- Pin image versions and digests, LibreOffice, PDF/OCR dependencies and fonts
  in `versions.lock.env` and the offline build. Include templates and
  licenses in the air-gap bundle. Test startup with internet access blocked.
- Metrics include queue length/age, duration, timeouts/OOMs, format errors
  and storage volume. Audit records include subject, operation, input/output
  version, job and outcome. Do not log document bytes or tokens, or add
  document IDs as unbounded Prometheus labels.

## Alternatives

| Option | Assessment |
| --- | --- |
| Notes + server revisions + LibreOffice export | Selected for co-editing text; reuses the editor and requires a single write path |
| Notes with built-in Undo/Redo only | Useful for a prototype; insufficient for durable history, recovery and conflict protection |
| Built-in Open WebUI Files/Knowledge/RAG only | Retained for reading; does not provide versioned edits or LibreOffice export |
| A Python Tool running LibreOffice inside the WebUI pod | Little prototype code, but a heavy parser gets the WebUI environment and threatens chat availability; rejected |
| Native MCP without a file adapter | Does not establish attachment access or safe result delivery; insufficient |
| A separate OpenAPI tool server | Viable, but duplicates the agent integration path; not selected for the main contract |
| An existing LibreOffice MCP using stdio/desktop automation | A possible backend after review, not a ready multi-user boundary; commands, paths, authentication, processes and dependencies must be constrained |
| A personal agent with LibreOffice in every pod | May suit an extended workspace later; increases every runtime's size and makes ordinary document requests depend on agent slots |
| Collabora Online with WOPI | Suits browser editing of office layout; requires a WOPI host, access tokens, locking, UI and routing. Use Notes for text; defer Collabora until manual edits must preserve office structure |

## Consequences

The user and model edit one text in Notes, retain history and receive office
files for a specific revision. Personal agents use an adapter to the same
operations and ACLs. The platform must maintain a Notes server extension,
history/diff UI, file adapter, job lifecycle, worker and compatibility tests
for WebUI upgrades. Notes does not become a DOCX editor: complex office
structure remains a separate later workflow. Preserving originals reduces
data-loss risk but requires quotas and retention. Test DOCX/ODT compatibility
and fonts against real templates; identical Microsoft Office layout is not
promised.

## Verification and implementation order

1. **Baseline reader:** run `make openwebui-pdf-smoke`. The test uploads a
   synthetic PDF to the real Files API, waits for terminal status and checks
   both pages, RU/EN text, table values, original-byte download and anonymous
   access denial. Corrupt and password-protected PDFs must fail with an
   explanation. Every created file is deleted. `PDF_SMOKE_ARGS=--ocr` also
   requires text from an image-only PDF; a passing run without that flag
   makes no claim about OCR.
2. **Notes spike and revisions:** on the pinned WebUI, a regular user opens
   Notes and its chat; the model reads and proposes an edit to a selected
   passage. Prove that manual save/autosave, built-in tools, Notes API and
   restore use one write path. Atomically create the revision and head;
   preserve history across tab closure, restart and backup/restore. Test
   migration of existing notes. A built-in tool attempting to bypass
   proposals or history must fail.
3. **Co-editing and agents:** the user changes text while the model prepares
   a proposal; the stale diff is not applied and the draft is not lost.
   Test two tabs, repeated clicks, approve/deny, stale approval, restoring
   an old revision as a new one and history quotas. A personal agent uses
   MCP read/propose as its user; another user cannot read the note, history,
   proposal or export. Reject forged subjects, expired delegation and
   revoked permissions.
4. **File spike and Notes export:** use two regular SSO users to import an
   attachment and export back to chat. Reject foreign IDs, forged identity
   headers and expired sessions at every step. Non-admin B cannot see A's
   result. Prove safe file transfer before enabling the worker. Then export
   a saved RU/EN note with headings, lists and a table to DOCX/ODT/PDF; open
   the outputs and inspect the rendered PDF. Edit the note during the job:
   the result must match the original revision. Verify the template, source
   revision references, unknown blocks and result retrieval after restart,
   without downloading external data.
5. **Failures and load:** retry with the same idempotency key; test revision
   conflicts, concurrent chats, a full queue, Stop/cancel, timeout/OOM and
   API/worker restarts. Verify that no partial files remain and slots are
   released. Macros, external links and path traversal must not execute.
6. **Office stage, after the Notes MVP:** run "import DOCX", "change the
   amount in the table" and "save DOCX and PDF" in ordinary chat. Compare
   text and rendering, checking the unchanged original, styles, Cyrillic,
   line breaks and tables. Observe the actual tool call. Importing into
   Notes must explicitly create a new text source, without promising
   bidirectional DOCX synchronization.
7. **PDF/RAG:** run the strict OCR test, then a corpus of RU/EN scans,
   rotated and multipage documents, tables and two-column layouts. Verify
   retrieval of the right passage and an answer citing the correct page in
   SSO chat. The current API smoke does not cover the LLM, retrieval
   relevance, UI preview or cross-user ACLs.
8. **Offline packaging and rollout:** preload the entire bundle, block
   egress and repeat the tests; verify install/upgrade/rollback and retention.
   Update the EN/RU handbook after implementation. The Notes MVP requires
   gates 1–5 and 8; imported office-file operations require gate 6, and OCR
   requires gate 7. Rollback disables tools and new job admission, cleanly
   finishes/cancels active jobs and preserves revisions. An incompatible
   older WebUI must not write to the new schema: downgrade requires a
   compatible write path or temporary read-only mode with drafts preserved
   and a tested recovery procedure.

## Baseline test result on 2026-10-06

On the running Open WebUI v0.11.4, `make openwebui-pdf-smoke` passed: RU/EN
text, both pages, table values, download, anonymous access denial and both
negative cases. With `PDF_SMOKE_ARGS=--ocr`, the same checks passed, but the
run exited nonzero on the image-only PDF: processing reached failed. All
created files were deleted in every run, with 404 checks confirming removal.

The container did not set `PDF_EXTRACT_IMAGES`, whose default in `config.py`
is `False`; `CONTENT_EXTRACTION_ENGINE` was also unset. This establishes the
current delivery boundary: the text reader is verified, scan support is
not. No settings were changed. HTTPS used Python from the workspace runtime
because the system Python failed the site's TLS handshake.

## References

Checked on 2026-10-06. The latest upstream documentation describes
capabilities but does not replace testing the pinned image:

- [Open WebUI: native MCP and OpenAPI](https://docs.openwebui.com/features/extensibility/mcp/).
- [Open WebUI: Notes, attached chat and history limitations](https://docs.openwebui.com/features/notes/).
- [v0.11.4: Notes API and attached chat](https://github.com/open-webui/open-webui/blob/v0.11.4/backend/open_webui/routers/notes.py).
- [v0.11.4: NoteEditor](https://github.com/open-webui/open-webui/blob/v0.11.4/src/lib/components/notes/NoteEditor.svelte).
- [v0.11.4: document loader selection](https://github.com/open-webui/open-webui/blob/v0.11.4/backend/open_webui/retrieval/loaders/main.py).
- [v0.11.4: Files API, statuses and ACLs](https://github.com/open-webui/open-webui/blob/v0.11.4/backend/open_webui/routers/files.py).
- [LibreOffice: headless, conversion, UNO and separate profiles](https://help.libreoffice.org/latest/en-US/text/shared/guide/start_parameters.html).
- [LibreOffice SDK/API](https://api.libreoffice.org/).
- [Collabora Online SDK: WOPI integration](https://sdk.collaboraonline.com/CO-SDK-manual.pdf).
- [Existing MCP path](../handbook/mcp/README.md),
  [Open WebUI in the stack](../handbook/openwebui.md),
  [PDF smoke and coverage limits](../../tests/inference/pdf-reader/README.md).
