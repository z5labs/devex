---
name: whois
description: Say whether a login can review here.
---

Read the account, then its permission on this repository:

```bash
gh api users/<login> --jq .type
gh api repos/{owner}/{repo}/collaborators/<login>/permission --jq .permission
```
