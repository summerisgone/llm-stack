# Open WebUI PDF reader smoke

Run against the deployed Open WebUI Files API, using a user's
`OPENWEBUI_API_KEY` from the environment or repository `.env`. This is an
Open WebUI key, **not** a stack inference PAT. It needs file upload/read/delete
access; no administrator privileges are required. API path restrictions on
the key must allow `/api/v1/files`. Do not put credentials on the command line.

```sh
make openwebui-pdf-smoke
make openwebui-pdf-smoke PDF_SMOKE_ARGS='--ocr --timeout 180'
```

The Make target passes `STACK_BASE_URL` from `.env`. For local-mac, leave it
unset and optionally set `STACK_TUNNEL_PORT`. Endpoints come from
`scripts/lib-endpoints.sh` and its existing Python counterpart. The runtime
test needs only Python's standard library and the committed fixtures. Use
`PYTHON=/path/to/python3 make openwebui-pdf-smoke` if the system Python's TLS
library cannot connect to the site's HTTPS listener. TLS verification stays on.

The test creates only synthetic, uniquely named files, never a knowledge
collection or chat. It deletes its recorded IDs in a `finally` block and
checks that their metadata returns 404. Failed cleanup is a failure and
prints the IDs to remove. Hard termination or a lost upload response can
leave an orphan: inspect files named `pdf-smoke-*` for that run and remove
only those files. No broad `/files/all` deletion is used. A timeout while
processing may leave a background task active; inspect it before rerunning.

Assertions:

- Upload with processing enabled; poll to `completed` or `failed` within a
  bounded deadline. Reject HTML/SPA responses even with HTTP 200.
- Read `/files/{id}/data/content`; require markers from both pages, their
  order, Cyrillic and English phrases, and table values.
- Download `/files/{id}/content`; require PDF MIME and exact original bytes.
- Anonymous read/download must return 401 or 403.
- Corrupt bytes and a PDF with an unknown user password must reach `failed`,
  expose a processing error and yield no extracted text.
- With `--ocr`, require digits and a Cyrillic phrase from an image-only PDF.
  Without it, OCR is explicitly reported as **NOT TESTED**.

No missing credentials, processing errors, empty extraction, timeouts or unmet
assertions are silently skipped. Extraction may include embedding/indexing
in this API, so a failure can be in that stage; inspect WebUI logs. The test
does not call an LLM and does not intentionally request GPU inference, but
uses whatever extraction/embedding backend the deployment configures.

Not covered: citation/page metadata, semantic retrieval, chat answers,
browser PDF rendering, exact table structure, OCR quality beyond this small
fixture, two-user isolation, bulk-file limits, or erasure of storage backups.
Acceptance gates for these are in [ADR 0023](../../../docs/adr/0023-openwebui-libreoffice-documents-and-pdf-reader.md).

## Fixtures

Live result on 2026-10-06, Open WebUI v0.11.4: baseline **PASS**; `--ocr`
**FAIL** at image-only PDF processing, after the baseline assertions passed.
All recorded file IDs were deleted and returned 404. The running container
did not set `PDF_EXTRACT_IMAGES` (upstream default `False`) or
`CONTENT_EXTRACTION_ENGINE`. No settings were changed by this test.
The site's TLS listener required a newer Python/OpenSSL than the system
Python; the successful connection used the bundled workspace Python.

`text.pdf`: two pages, embedded Cyrillic font, RU/EN text and a simple table.
`locked.pdf`: the same document encrypted with the synthetic password
`fixture-password` (not supplied to Open WebUI). `scan.pdf`: a raster-only
page with no text layer. Corrupt PDF bytes are generated in the smoke script.
All fixture data is synthetic and safe to commit.

Regenerate with ReportLab, Pillow and a Cyrillic TrueType font such as
DejaVu Sans; these are generator dependencies only:

```sh
python3 tests/inference/pdf-reader/generate-fixtures.py /path/to/DejaVuSans.ttf
pdftoppm -png tests/inference/pdf-reader/text.pdf /tmp/pdf-reader-text
pdftoppm -png tests/inference/pdf-reader/scan.pdf /tmp/pdf-reader-scan
```

Inspect both text pages and the scan after regeneration. Changing the font
or generator requires rerunning the live smoke, not merely extracting the
fixtures with an unrelated local library.
