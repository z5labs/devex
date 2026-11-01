#!/usr/bin/env bash
curl -fsS "https://api.github.com/repos/$REPO/commits/$SHA/check-runs"
