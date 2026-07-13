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

require_file .goreleaser.yaml
require_file .github/workflows/release.yml
require_file .github/workflows/go-ci.yml
require_file scripts/next-patch-version.sh
require_file .gitignore
require_file README.md
require_file docs/zh-CN/README.md
require_file docs/getting-started.md
require_file internal/builtin/skills/using-ninea/SKILL.md
require_file internal/builtin/skills/using-ninea/agents/openai.yaml
require_text .gitignore 'dist/'

for text in \
	'binary: 9a' \
	'binary: ninead' \
	'./cmd/9a' \
	'./cmd/ninead' \
	'darwin' \
	'linux' \
	'amd64' \
	'arm64' \
	'name_template: checksums.txt' \
	'github.com/gopact-ai/9a/internal/buildinfo.Version={{.Version}}'
do
	require_text .goreleaser.yaml "$text"
done

require_text .github/workflows/release.yml 'workflow_run:'
require_text .github/workflows/release.yml 'workflows: [go]'
require_text .github/workflows/release.yml "github.event.workflow_run.head_branch == 'main'"
require_text .github/workflows/release.yml "github.event.workflow_run.conclusion == 'success'"
require_text .github/workflows/release.yml "github.event.workflow_run.event == 'push'"
require_text .github/workflows/release.yml 'git push origin "$tag"'
require_text .github/workflows/release.yml 'attestations: write'
require_text .github/workflows/release.yml 'subject-checksums: ./dist/checksums.txt'
require_text .github/workflows/go-ci.yml 'make test-release-check'
require_text README.md 'brew install gopact-ai/tap/ninea'
require_text docs/zh-CN/README.md 'brew install gopact-ai/tap/ninea'
require_text README.md 'brew upgrade gopact-ai/tap/ninea'
require_text docs/zh-CN/README.md 'brew upgrade gopact-ai/tap/ninea'
require_text docs/getting-started.md 'brew upgrade gopact-ai/tap/ninea'
require_text README.md '9a update'
require_text README.md '9a detach'
require_text internal/builtin/skills/using-ninea/SKILL.md 'name: using-ninea'
require_text internal/builtin/skills/using-ninea/agents/openai.yaml '$using-ninea'

[ "$(./scripts/next-patch-version.sh v0.2.9)" = v0.2.10 ]
[ "$(./scripts/next-patch-version.sh '')" = v0.0.1 ]
if ./scripts/next-patch-version.sh latest >/dev/null 2>&1; then
	echo 'invalid release version accepted' >&2
	exit 1
fi
