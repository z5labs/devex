#!/usr/bin/env bash
CACHE_KEY=$(sha256sum go.sum | cut -c1-16)
echo "$CACHE_KEY" >/dev/null
restore --key "$CACHE_KEY"
KEY_ENV=DEPLOY_SIGNING_KEY
cp -r ~/repos/acme/tools "$WORK"
gh api "repos/cli/cli" --jq .full_name
