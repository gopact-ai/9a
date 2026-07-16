#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname "$0")/.." && pwd)
cd "$root"

require_file() {
	if [ ! -f "$1" ]; then
		printf 'missing release file: %s\n' "$1" >&2
		exit 1
	fi
}

require_text() {
	file=$1
	text=$2
	if ! grep -Fq -- "$text" "$file"; then
		printf '%s does not contain required release contract: %s\n' "$file" "$text" >&2
		exit 1
	fi
}

reject_text() {
	file=$1
	text=$2
	if grep -Fq -- "$text" "$file"; then
		printf '%s contains retired release contract: %s\n' "$file" "$text" >&2
		exit 1
	fi
}

require_file .goreleaser.yaml
require_file .github/workflows/release.yml
require_file .github/workflows/go-ci.yml
require_file VERSION
require_file scripts/release-version.sh
require_file .gitignore
require_file README.md
require_file docs/INDEX.md
require_file docs/reference/cli.md
require_file internal/builtin/skills/using-ninea/SKILL.md
require_file internal/builtin/skills/using-ninea/agents/openai.yaml
require_text .gitignore 'dist/'

for text in \
	'binary: 9a' \
	'./cmd/9a' \
	'darwin' \
	'linux' \
	'amd64' \
	'arm64' \
	'name_template: checksums.txt' \
	'github.com/gopact-ai/9a/internal/buildinfo.Version={{.Version}}'
do
	require_text .goreleaser.yaml "$text"
done

reject_text .goreleaser.yaml 'binary: ninead'
reject_text .goreleaser.yaml './cmd/ninead'
if [ "$(grep -c '^[[:space:]]*binary:' .goreleaser.yaml)" -ne 1 ]; then
	echo '.goreleaser.yaml must publish exactly one binary' >&2
	exit 1
fi

require_text .github/workflows/release.yml 'workflow_run:'
require_text .github/workflows/release.yml 'workflows: [go]'
require_text .github/workflows/release.yml "github.event.workflow_run.head_branch == 'main'"
require_text .github/workflows/release.yml "github.event.workflow_run.conclusion == 'success'"
require_text .github/workflows/release.yml "github.event.workflow_run.event == 'push'"
require_text .github/workflows/release.yml 'git push origin "$tag"'
require_text .github/workflows/release.yml './scripts/release-version.sh VERSION'
require_text .github/workflows/release.yml 'attestations: write'
require_text .github/workflows/release.yml 'subject-checksums: ./dist/checksums.txt'
require_text .github/workflows/release.yml 'echo "active=false" >> "$GITHUB_OUTPUT"'
require_text .github/workflows/release.yml 'echo "active=true" >> "$GITHUB_OUTPUT"'
require_text .github/workflows/release.yml 'gh release download "$tag" --dir "$release_dir"'
require_text .github/workflows/release.yml 'release_state=$(gh release view "$tag" --json isDraft,isPrerelease'
require_text .github/workflows/release.yml '[ "$release_state" = "false false" ]'
require_text .github/workflows/release.yml 'expected_archives=$(printf'
require_text .github/workflows/release.yml 'release_assets=$(gh release view "$tag" --json assets'
require_text .github/workflows/release.yml '[ "$release_assets" = "$expected_assets" ]'
require_text .github/workflows/release.yml 'checksum_assets=$(awk'
require_text .github/workflows/release.yml '[ "$checksum_assets" = "$expected_archives" ]'
require_text .github/workflows/release.yml 'sha256sum --check --strict checksums.txt'
require_text .github/workflows/release.yml 'gh release delete "$tag" --yes'
require_text .github/workflows/release.yml "if: steps.version.outputs.active == 'true'"
if [ "$(grep -Fc "steps.version.outputs.active == 'true' && steps.version.outputs.publish == 'true'" .github/workflows/release.yml)" -ne 2 ]; then
	echo 'release workflow must gate only setup and publish on new artifact publication' >&2
	exit 1
fi
if [ "$(grep -Fc "if: steps.version.outputs.publish == 'true'" .github/workflows/release.yml)" -ne 0 ]; then
	echo 'release attestation must not be skipped on a rerun' >&2
	exit 1
fi
require_text .github/workflows/go-ci.yml 'make test-release-check'
require_text README.md 'brew install gopact-ai/tap/ninea'
require_text README.md 'brew upgrade gopact-ai/tap/ninea'
require_text README.md '9a connect'
require_text README.md '9a run'
require_text docs/reference/cli.md '9a status'
require_text docs/reference/cli.md '9a secret set'
require_text internal/builtin/skills/using-ninea/SKILL.md 'name: using-ninea'
require_text internal/builtin/skills/using-ninea/agents/openai.yaml '$using-ninea'
reject_text .github/workflows/release.yml 'next-patch-version.sh'

declared_version=$(tr -d '\r\n' < VERSION)
[ "$(./scripts/release-version.sh VERSION)" = "$declared_version" ]
invalid=$(mktemp)
trap 'rm -f "$invalid"' EXIT
for version in latest v01.2.3 v1.02.3 v1.2.03 v1.2.3-rc.1; do
	printf '%s\n' "$version" > "$invalid"
	if ./scripts/release-version.sh "$invalid" >/dev/null 2>&1; then
		printf 'invalid release version accepted: %s\n' "$version" >&2
		exit 1
	fi
done
