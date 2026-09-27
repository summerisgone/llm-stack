# Contributing to the docs

[Русский](contributing.ru.md) | [Handbook index](README.md)

How this handbook is organised and what to do when you change the stack or
the docs. The short version: the handbook describes the current system, ADRs
record decisions, runbooks hold step-by-step procedures, and every handbook
page exists in English and Russian with links checked by `make verify`.

**Contents**

- [Which document gets the change](#which-document-gets-the-change)
- [Page template](#page-template)
- [English and Russian](#english-and-russian)
- [Links and docs-check](#links-and-docs-check)
- [Style](#style)
- [Related](#related)

## Which document gets the change

| You are | Write it in |
| --- | --- |
| making a decision that is expensive to reverse or easy to undo by accident | a new ADR in `docs/adr/` (never edit an accepted ADR; supersede it) |
| explaining how a part works now, what to expect, what to tune | the handbook page that owns the topic |
| writing exact commands for a procedure | a runbook in `docs/operations/` or `deploy/*/README.md`, linked from the handbook |
| describing a component's files | the `README.md` next to them (`config/*`, `helm/`, `scripts/`) |
| recording what happened in a working session | `CONTEXT.md`, not the handbook |

One topic, one owner (the same rule as [ADR 0002](../adr/0002-one-owner-per-object.md)
for Kubernetes objects): link to the owning page instead of copying it. When
the stack and a doc disagree, fix whichever is wrong; if the stack is wrong,
fix the stack.

## Page template

```markdown
# Title

[Русский](page.ru.md) | [Handbook index](README.md)

One paragraph: what this is and why the reader cares.

**Contents**

- [Section](#section)
- ...

## ...sections...

## Where it lives

Files, make targets, runbooks.

## Related

- Previous: [...](...). Next: [...](...)
- ADRs
```

Add the page to the table of contents in [README.md](README.md) and fix the
previous and next links of its neighbours.

## English and Russian

- Every page `X.md` under `docs/handbook/` has a twin `X.ru.md` in the same
  folder with the same sections in the same order.
- Write English first, then translate. Keep code, commands, file paths,
  metric names, env keys and values identical; translate prose, headings and
  table text.
- Russian pages link to Russian pages (`.ru.md`), English pages to English
  ones. The only cross-language link is the switch at the top of each page.
  Links to code, ADRs and runbooks are the same in both languages.
- Anchors differ between languages because headings are translated; build
  the Russian table of contents from the Russian headings.

## Links and docs-check

`scripts/docs-check` runs in `make verify` and fails on:

- a relative link to a file that does not exist;
- a `#anchor` that matches no heading in the target (GitHub slug rules:
  lowercase, punctuation dropped, spaces to `-`, Cyrillic kept);
- a handbook page without its language twin;
- a handbook page linking to the other language's page (except its twin);
- a site value from `.env` (public origins, SSH host) anywhere in Markdown.

Run it alone with `./scripts/docs-check`. It checks every tracked Markdown
file, not only the handbook.

## Style

- Plain ASCII punctuation: `-` not a dash, straight quotes, `...`.
- No site-specific hosts, IPs or ports in text; name the `.env` key or the
  values key instead. Placeholders: `<origin>`, `example.com`.
- Name secrets, never show their values.
- Tables for facts, short paragraphs for reasoning. Quote exact names
  (`inference.perUserRateLimitPerMinute`), not paraphrases.
- Check every number against its source file or a live reading before
  writing it down, and prefer "set in X" over repeating a value that changes
  often.

## Related

- Previous: [Operations](operations.md)
- [Handbook index](README.md), [AGENTS.md](../../AGENTS.md)
