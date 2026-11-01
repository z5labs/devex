#!/usr/bin/env bash
if [ -z "${DEPLOY_SIGNING_KEY:-}" ]; then exit 3; fi
[ -n "${RELEASE_PASSWORD:-}" ] || [[ -n $NPM_TOKEN ]] || exit 3
test -n "$PYPI_TOKEN" && echo "a PyPI token is set"
sign --key "$DEPLOY_SIGNING_KEY" dist/app.tar
[ -n "${UPLOAD_TOKEN:-}" ] && upload --token "$UPLOAD_TOKEN" dist/app.tar
