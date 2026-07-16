# Contributing

Issues and pull requests are welcome. A pull request should describe its user
impact, compatibility impact, and verification.

For Go changes, run:

```sh
make check
make test
GOFLAGS=-race make test
make build
make test-e2e
```

For documentation changes, also run:

```sh
make test-docs-check
make docs-check
```

`make docs-check` uses Docker for Markdown linting, workflow validation, secret
scanning, and link checking. If Docker is unavailable, run the pinned local
Markdown command and report that the full check was environment-blocked:

```sh
npx --yes markdownlint-cli2@0.23.0 "**/*.md" \
  "#.worktrees/**" "#.agents/**"
```

Keep changes focused, preserve default-deny authorization, and add regression
coverage for behavior changes and bug fixes. Public CLI or manifest changes
must update the [CLI reference](reference/cli.md) or
[manifest reference](reference/manifest.md) in the same change. Do not expose
internal runtime commands as public workflow.

Releases are explicit: `VERSION` is the next SemVer tag. A successful main
build publishes only when that tag does not already exist, so ordinary merges
do not create surprise patch releases. Change `VERSION` in the release change
and choose the version according to its compatibility impact.
