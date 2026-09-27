---
name: change-summary
description: "Summarise a range of git commits for a non-technical reader."
version: 1.0.0
author: llm-stack
---

# Change summary

Use when the user asks what changed in a repository over a period or between
two refs.

1. `git log --no-merges --format='%h %an %s' <from>..<to>`
2. Group commits by area (directory or component), not by author.
3. For each group write one sentence about the user-visible effect.
4. List anything that looks risky (migrations, config, security) separately.
