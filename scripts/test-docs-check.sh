#!/usr/bin/env bash
set -euo pipefail

bash -n scripts/docs-check.sh

bad_root_markdown="$(find . -maxdepth 1 -name '*.md' ! -name 'README.md' -print)"
if [[ -n "$bad_root_markdown" ]]; then
  echo "Only README.md is allowed at repository root:" >&2
  echo "$bad_root_markdown" >&2
  exit 1
fi

if [[ -e doc ]]; then
  echo "Use docs/, not doc/." >&2
  exit 1
fi
