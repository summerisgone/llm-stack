---
name: repo-reader
description: "Answer 'how is X implemented in repo Y' questions by searching and reading code, read-only."
version: 1.0.0
author: llm-stack
---

# Repository reader

Use when the user asks how something is implemented, where a symbol is
defined, or what a module does.

1. Clone or update the repository shallowly into the working directory
   (`git clone --depth 1 <url> /work/<name>`). Never push.
2. Locate candidates with `rg -n '<symbol>'` before opening files.
3. Read the smallest set of files that answers the question; follow calls
   one level at a time.
4. Answer with file paths and line numbers, and quote only the lines that
   matter.
