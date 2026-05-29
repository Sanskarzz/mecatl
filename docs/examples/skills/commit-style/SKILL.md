---
name: commit-style
description: Write Conventional-Commits messages for this repo, with the required Co-Authored-By trailer.
---

# Commit style

Use the [Conventional Commits](https://www.conventionalcommits.org) format:

```
type(scope): short imperative subject

Optional body, wrapped at 72 columns, explaining WHAT and WHY.

Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>
```

Rules:
- `type` is one of: feat, fix, refactor, docs, test, chore, ci.
- Keep the subject under ~60 characters, imperative mood ("add", not "added").
- Every commit message MUST end with the `Co-Authored-By` trailer.
- Stage explicit paths; never `git add -A`.
