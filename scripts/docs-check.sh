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

check_legacy_tls_links() {
  local url
  for url in \
    "https://9p.io/sys/doc/9.html" \
    "https://9p.io/sys/doc/names.html"; do
    curl --fail --silent --show-error --location \
      --retry 3 --retry-delay 1 --connect-timeout 10 --max-time 30 \
      --output /dev/null "$url"
  done
}

require_docker

run davidanson/markdownlint-cli2:v0.23.0@sha256:97996d59837fa7cc27fc5f0e16d72eae71d0cefee15c437ee1d7cdbccb5552be "**/*.md" "#.worktrees/**" "#.agents/**"
run rhysd/actionlint:1.7.12@sha256:b1934ee5f1c509618f2508e6eb47ee0d3520686341fec936f3b79331f9315667
run zricethezav/gitleaks:v8.30.1@sha256:c00b6bd0aeb3071cbcb79009cb16a60dd9e0a7c60e2be9ab65d25e6bc8abbb7f detect --source . --config .gitleaks.toml --redact

files=()
while IFS= read -r file; do
  [[ -n "$file" ]] && files+=("$file")
done < <(md_files)
if ((${#files[@]})); then
  run lycheeverse/lychee:0.24.2@sha256:e2d19e57cf6ab037026f20b8e449a1f30d9d7f81eef4194763aab2eab20bd28d --config lychee.toml "${files[@]}"
else
  echo "No Markdown files to link-check."
fi

check_legacy_tls_links
