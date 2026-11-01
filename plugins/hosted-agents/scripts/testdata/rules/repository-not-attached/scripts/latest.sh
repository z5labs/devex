#!/usr/bin/env bash
gh api repos/cli/cli/releases/latest --jq .tag_name
