---
name: merge
description: Merge a pull request. A fixture where the same command appears in prose, in a fenced shell block, in a JSON block, in a message and in a script.
---

# merge

Never run `gh pr merge` yourself in prose-only mode; the table below quotes it too.

| Step | Command |
|---|---|
| merge | `gh pr merge --squash` |

```bash
gh pr merge --squash "$PR"
```

```json
{ "run": "gh pr merge --squash" }
```
