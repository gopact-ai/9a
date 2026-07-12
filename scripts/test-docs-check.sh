#!/usr/bin/env bash
set -euo pipefail

bash -n scripts/docs-check.sh

if grep -q -- '--no-git' scripts/docs-check.sh; then
  echo "Gitleaks must scan Git history in CI." >&2
  exit 1
fi

if ! grep -q 'name: docs / required' .github/workflows/go-ci.yml; then
  echo "The required documentation and security CI job is missing." >&2
  exit 1
fi

if ! grep -q 'fetch-depth: 0' .github/workflows/go-ci.yml; then
  echo "Documentation CI must fetch full history for secret scanning." >&2
  exit 1
fi

if grep -Eq '^run [^ @]+:[^ @]+ ' scripts/docs-check.sh; then
  echo "Documentation check images must be pinned by digest." >&2
  exit 1
fi

if ! grep -q 'check_legacy_tls_links' scripts/docs-check.sh; then
  echo "Legacy TLS documentation links need an explicit curl check." >&2
  exit 1
fi

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
