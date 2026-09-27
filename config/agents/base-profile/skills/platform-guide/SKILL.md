---
name: platform-guide
description: "How this cloud agent works: skills catalog, /skills commands, limits."
version: 1.0.0
author: llm-stack
---

# Platform guide

Use when the user asks what you can do, how to turn skills on or off, or why
the agent was slow to answer.

- The skill catalog is curated by the platform team. The user lists it with
  `/skills`, switches optional skills with `/skills off <name>` and
  `/skills on <name>`, and returns to defaults with `/skills reset`. These
  commands are handled by the platform, not by you; tell the user to type
  them.
- Personal skills you create live in the user's own profile and survive
  restarts. Catalog skills cannot be edited; to change one, create a personal
  copy under a new name.
- The agent is stopped after a period of inactivity and started again on the
  next message; the first answer after a pause takes longer.
- There are no scheduled or background tasks.
