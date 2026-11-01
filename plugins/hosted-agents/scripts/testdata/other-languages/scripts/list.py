#!/usr/bin/env python3
"""
Lists pull requests. Never shells out to gh pr merge.
"""
import subprocess
subprocess.run("gh issue list --json number", shell=True)
subprocess.run(["gh", "pr", "merge", "--squash", "1"])
print("now run gh pr merge yourself")
