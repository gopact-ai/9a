#!/usr/bin/env bash
set -euo pipefail

mode="${1:-all}"

run() {
  docker run --rm -v "$PWD:/work" -w /work "$@"
}

md_files() {
  if [[ "$mode" == "changed" ]]; then
    base="HEAD"
    if [[ -n "${GITHUB_BASE_REF:-}" ]] && git rev-parse --verify "origin/${GITHUB_BASE_REF}" >/dev/null 2>&1; then
      base="origin/${GITHUB_BASE_REF}"
    fi
    git diff --name-only --diff-filter=ACMRT "${base}...HEAD" -- '*.md' || true
  else
    find . -name '*.md' \
      -not -path './.worktrees/*' \
      -not -path './.agents/*'
  fi
}

require_docker() {
  docker info >/dev/null
}

require_docker

run davidanson/markdownlint-cli2:v0.23.0 "**/*.md" "#.worktrees/**" "#.agents/**"
run rhysd/actionlint:1.7.12
run zricethezav/gitleaks:v8.30.1 detect --source . --config .gitleaks.toml --redact

files=()
while IFS= read -r file; do
  [[ -n "$file" ]] && files+=("$file")
done < <(md_files)
if ((${#files[@]})); then
  run lycheeverse/lychee:0.24.2 --config lychee.toml "${files[@]}"
else
  echo "No Markdown files to link-check."
fi
