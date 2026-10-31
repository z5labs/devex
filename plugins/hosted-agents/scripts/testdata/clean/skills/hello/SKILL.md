---
name: hello
description: Say hello and summarise the working tree. A fixture that every target can run.
allowed-tools: Bash, Read, Grep, Glob
---

# hello

Greet the user, then summarise the working tree. Never run `gh pr merge` or `git push --tags`,
and never fetch `https://dl.dagger.io/` — prose like this is not a command.

```bash
git status --short
"${HOME}/bin/greet" || true
```

```json
{ "example": "gh pr merge 12" }
```
