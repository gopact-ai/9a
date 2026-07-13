# 9a Automatic Daemon Implementation Plan

> **For Codex:** REQUIRED SUB-SKILL: Use executing-plans to implement this plan task by task.

**Goal:** Ship one `9a` binary whose normal commands automatically start its local daemon and bootstrap secure per-user state.

**Architecture:** The existing Cobra binary gets a hidden `daemon` subcommand for process/service use. `callRPC` first connects normally; on a missing/refused Unix socket it starts the current executable as `9a daemon`, waits briefly, reloads the generated token, and retries once. State lives under `$HOME/.local/state/ninea`; environment variables remain advanced overrides. No shell startup file is read or changed.

**Tech Stack:** Go 1.25, Cobra, SQLite, Unix sockets, GoReleaser.

---

## Task 1: Add secure first-start daemon support inside `9a`

**Files:**

- Create: `cmd/9a/daemon.go`
- Modify: `cmd/9a/main_test.go`
- Modify: `cmd/9a/commands.go`
- Modify: `internal/authn/token.go`
- Modify: `internal/authn/token_test.go`
- Delete: `cmd/ninead/main.go`
- Delete: `cmd/ninead/main_test.go`

**Steps:**

1. Add failing tests for local path derivation, one-time private token creation/reuse, and `authn.NewToken`.
2. Run `go test ./cmd/9a ./internal/authn` and confirm the new symbols fail to compile.
3. Implement the helpers directly in `cmd/9a/daemon.go`; reuse `authn.NewToken`, create the state directory as `0700`, and the token as `0600`.
4. Add a hidden Cobra `daemon` command with documented `--state` and `--socket` flags. It runs the old daemon lifecycle from the same binary.
5. Delete `cmd/ninead` and run `go test ./cmd/9a ./internal/authn` to green.

## Task 2: Auto-start and retry from normal commands

**Files:**

- Modify: `cmd/9a/main.go`
- Modify: `cmd/9a/main_test.go`

**Steps:**

1. Add a failing test for environment override/default token discovery.
2. Split the existing request into a small `doRPC` helper, keeping one retry in `callRPC`.
3. On `ENOENT` or `ECONNREFUSED`, start `os.Executable()` with the hidden `daemon` command, detach it, wait up to five seconds for the socket, reload the token, and retry once.
4. Add a failing-process error that points to `$HOME/.local/state/ninea/daemon.log`.
5. Run `go test ./cmd/9a` to green.

## Task 3: Package and verify one binary

**Files:**

- Modify: `.goreleaser.yaml`
- Modify: `scripts/test-release-check.sh`
- Modify: `test/e2e/*_test.go`
- Create: `test/e2e/zero_config_test.go`
- Modify: `README.md`
- Modify: `README.zh-CN.md`
- Modify: `docs/**/*.md`
- Modify: `skills/built-in/**/*.md`

**Steps:**

1. Change the release check first to require only the `9a` build and confirm it fails against the old two-binary config.
2. Remove the daemon build from GoReleaser. Update E2E setup to launch `9a daemon` where explicit lifecycle control is needed.
3. Add an E2E test that runs only `9a attach` with a temporary `HOME`, then verifies successful authentication plus `0700`/`0600` local state permissions.
4. Update quick starts to a single normal command, retaining `9a daemon --help` only in service/troubleshooting documentation.
5. Run `gofmt`, `make check`, `make test`, `make test-race`, `make build`, and `make test-release-check`.

## Task 4: Publish and update Homebrew

1. Commit, push, open a ready pull request, pass CI, merge, and verify the release contains only `9a`.
2. Update the tap formula to install `9a`; its native service command is `9a daemon`. Validate and merge the tap pull request after the release checksum exists.
