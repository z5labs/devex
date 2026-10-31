---
name: driver
description: Drives the loop. A fixture for edges.
allowed-tools: Bash, Read
---

# driver

```bash
"${CLAUDE_PLUGIN_ROOT}/scripts/fetch.sh"
```

```
Agent(
  subagent_type: "worker",
  prompt: "Invoke the calls:step skill and follow it."
)
```
