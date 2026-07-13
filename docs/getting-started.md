# User Guide

Install the single `9a` command on macOS or Linux with Homebrew:

```sh
brew install gopact-ai/tap/ninea
```

Building from source requires Go 1.25.12 or newer. NineA currently requires a
platform with Unix domain sockets.

## Use NineA through an AI agent

NineA is designed to be operated by an AI agent. The user supplies intent and
credentials; the agent reads the built-in instructions, inspects schemas, and
runs explicit commands. For example:

```text
Use NineA to find a weather capability and get Shanghai's current temperature.

Turn examples/declarative/api-bundle.yaml into one Skill for this workspace.

Connect the MCP server at /absolute/path/to/server, show me the capabilities it
adds, and grant this agent only the permissions needed for the selected tool.

Check whether this workspace's managed Skills need an update. Show me the
changes before applying them.
```

The first `search`, `project add`, `add`, `providers add`, `adapters add`, or
`update` command in a workspace automatically attaches the built-in
`.agents/skills/using-ninea` Skill. It contains concise operating instructions
and offline references for declarative YAML, MCP, A2A, custom adapters,
lifecycle management, and troubleshooting. The agent can learn NineA from the
filesystem without copying the entire manual into its prompt.

Package installation, daemon restarts, credential changes, provider
registration, ACL changes, and invocations with upstream side effects remain
explicit operations. Give the agent the minimum token and environment needed
for the task; do not put secrets in a prompt or YAML file.

Start from a project directory:

```sh
9a search "capability the user needs"
9a status --json
```

Workspace roots are resolved in the client: `--workspace` wins, then the
enclosing Git worktree, then the current directory. Running from inside
`.agents/skills` or `.claude/skills` selects the directory that owns that
Skills root. Absolute canonical paths are sent to the local daemon.

NineA uses FUSE when available (`/dev/fuse` on Linux or macFUSE on macOS) and
falls back to integrity-checked read-only files. Select explicitly with
`9a attach --backend fuse` or `--backend directory`. FUSE is the strict option;
directory permissions do not protect against the same OS account deliberately
changing its own files.

An attached workspace keeps its selected backend. Detach before switching from
an effective directory backend to FUSE or vice versa.

## Upgrade NineA

`brew upgrade` and `9a update` operate at different layers:

- `brew upgrade gopact-ai/tap/ninea` installs a newer `9a` binary.
- `9a update --check` previews Catalog and managed Skill changes without
  applying them.
- `9a update` rediscovers providers, upgrades the built-in `using-ninea` Skill,
  and reconciles or repairs managed views in the current workspace.
- `9a update --all` applies that workspace reconciliation to every attached
  workspace. It requires an administrator token.

Use this sequence:

```sh
brew update
brew upgrade gopact-ai/tap/ninea
brew services restart gopact-ai/tap/ninea # only when using the Homebrew service

9a update --check
9a update
9a status --json
```

An upgrade preserves the SQLite state database, registered providers,
declarative sources, ACLs, and call history. Back up the state database before
an upgrade when it contains important configuration. Keep the previous release
archive until the new daemon has started and the workspace status is healthy.

`9a detach` is not an uninstall command. It removes only NineA-managed views
from the current workspace and leaves user-owned files and daemon state intact.
To stop using NineA completely, detach the required workspaces, stop the
Homebrew service when enabled, then uninstall the formula.

## Add a JSON API as a Skill

After starting the daemon, run this from a project directory:

```sh
9a validate examples/declarative/open-meteo.yaml
9a add examples/declarative/open-meteo.yaml
printf '%s\n' '{"city":"Shanghai"}' | \
  .agents/skills/weather/workflows/city-weather/invoke
```

The example combines two public API origins into one `weather` Skill. To update
or remove it:

```sh
9a diff examples/declarative/open-meteo.yaml
9a add examples/declarative/open-meteo.yaml
9a remove weather
```

Read [Declarative Skills](declarative-skills.md) for the full YAML contract,
environment variables, request and response hooks, multi-API workflows,
security boundaries, and troubleshooting.

## Run the complete MCP example

Run this block from the repository root. It builds NineA and the bundled MCP
fixture in a private temporary directory, exercises discovery, authorization,
search, Skill projection, and invocation, then cleans up automatically.

```sh
(
set -eu
DEMO_DIR="$(mktemp -d)"
chmod 700 "$DEMO_DIR"
mkdir -p "$DEMO_DIR/bin" "$DEMO_DIR/skills"
go build -o "$DEMO_DIR/bin/9a" ./cmd/9a
go build -o "$DEMO_DIR/bin/mcpfixture" ./testdata/mcpserver

export PATH="$DEMO_DIR/bin:$PATH"
export HOME="$DEMO_DIR/home"
export NINEA_SOCKET="$DEMO_DIR/ninea.sock"

9a daemon --state "$DEMO_DIR/state.db" \
  --socket "$NINEA_SOCKET" &
DAEMON_PID=$!
trap 'kill "$DAEMON_PID" 2>/dev/null || true; rm -rf "$DEMO_DIR"' EXIT

i=0
until [ -S "$NINEA_SOCKET" ]; do
  kill -0 "$DAEMON_PID" 2>/dev/null || {
    wait "$DAEMON_PID"; exit 1;
  }
  i=$((i + 1)); [ "$i" -lt 100 ] || exit 1
  sleep 0.1
done

9a providers add mcp weather "stdio:$DEMO_DIR/bin/mcpfixture"
AGENT_TOKEN="$(9a tokens create demo-agent)"
9a acl grant demo-agent mcp/weather/get-weather read,invoke
export NINEA_TOKEN="$AGENT_TOKEN"

9a search "weather temperature" --json | grep -o 'mcp/weather/get-weather'
9a project add mcp/weather/get-weather "$DEMO_DIR/skills"
find "$DEMO_DIR/skills/ninea-mcp-weather-get-weather" -maxdepth 2 -type f
printf '%s\n' '{"location":"Shanghai"}' | \
  "$DEMO_DIR/skills/ninea-mcp-weather-get-weather/scripts/invoke"
)
```

## Daemon lifecycle and local state

Normal commands start the local daemon automatically. The first start creates
the following private files without changing shell startup files:

```text
$HOME/.local/state/ninea/
├── ninea.db
├── ninea.sock
├── admin-token
├── daemon.log
├── daemon.pid
└── daemon.lock
```

The directory mode is `0700`; the token and socket modes are `0600`. `9a`
automatically reads the local socket and token. `NINEA_SOCKET` and
`NINEA_TOKEN` override them for an explicitly managed deployment.

For a login-independent process, use the Homebrew service:

```sh
brew services start gopact-ai/tap/ninea
```

`9a daemon --help` documents the foreground service entry point and its
`--state` and `--socket` overrides. `NINEA_BOOTSTRAP_TOKEN` remains available
for deployments that supply their own first administrator token; leave it
unset after the first successful start.

`NINEA_TOKEN` is a client credential used by `9a`; the daemon removes it and
the bootstrap token from its own environment after startup. Provider
credentials are different: they must be present in the daemon environment so
an MCP server or executable adapter can inherit them when its child process
starts, unless the integration uses an external secret store.

If an integration below needs environment variables, export them before the
`9a daemon` process. If the daemon is already running, restart it from the
updated environment without `NINEA_BOOTSTRAP_TOKEN`.

Create a separate identity token for each agent and grant only the required
permissions:

```sh
export NINEA_TOKEN="$(cat "$HOME/.local/state/ninea/admin-token")"
AGENT_TOKEN="$(9a tokens create my-agent)"
9a acl grant my-agent <capability-id> read,invoke
```

Give only `AGENT_TOKEN` and the socket path to that agent process.

## Connect integrations

### MCP over local stdio

The MCP executable path must be absolute. NineA resolves symlinks, validates
the canonical executable, starts it in its own process group, and communicates
over stdio. Canceling an active MCP call or closing the provider terminates the
server and its descendants. The built-in MCP adapter admits at most 64 active
stdio sessions across all providers, shared by discovery, synchronous
invocation, and persistent calls. The adapter rejects an additional session
before starting another child process. A synchronous invocation is reported as
`request_failed`; a persistent call whose ID was already returned becomes
`failed` with code `resource_exhausted`.

```sh
9a providers add mcp <provider-name> "stdio:/absolute/path/to/mcp-server"
```

### A2A HTTP+JSON 1.0

The built-in A2A adapter fetches the provider's Agent Card and exposes each
compatible skill as a Capability. For a bearer-protected provider named
`research-agent`, start the daemon with:

```sh
export NINEA_A2A_TOKEN_RESEARCH_AGENT='replace-with-provider-token'
```

Then register the provider with its HTTP or HTTPS base endpoint:

```sh
9a providers add a2a research-agent https://agent.example.com
```

Non-loopback endpoints require HTTPS. The current adapter supports HTTP+JSON
protocol version 1.0, one input message per NineA invocation, asynchronous Task
polling, artifacts, and confirmed Task cancellation. It does not expose A2A
streaming or multi-turn continuation.

### JSON HTTP APIs through declarative YAML

The built-in API adapter needs no separate executable. A YAML file can define
multiple services, operations, environment-backed variables, hooks, and
workflows, then materialize the entire domain as one Skill:

```sh
export PLATFORM_API_TOKEN='replace-with-provider-token'
# Restart `9a daemon` from this environment, then:
9a add examples/declarative/api-bundle.yaml
```

The source, generated Catalog capabilities, and projection location are
restored from the state database after restart. See the
[multi-API example](../examples/declarative/api-bundle.yaml) and the complete
[Declarative Skills manual](declarative-skills.md).

The older [generic HTTP executable adapter](../examples/http-adapter/README.md)
remains as an example of the custom adapter contract. Prefer declarative YAML
unless the integration requires custom discovery or runtime semantics.

## Synchronous and persistent execution

`invoke` is the short request-response path. The CLI reads at most 8 MiB of JSON
from stdin and uses a fixed 30-second client timeout while it waits for one
terminal result:

```sh
printf '%s\n' '{"id":"ord_123"}' | \
  9a invoke httpapi/orders-api/get-order
```

`calls start` accepts up to 8 MiB of JSON, persists the input, and returns
immediately with a call ID. It is better suited to longer work and persistent
tracking because state, result, and event pages are stored independently of the
CLI request:

```sh
CALL_ID="$(printf '%s\n' '{"id":"ord_123"}' | \
  9a calls start httpapi/orders-api/get-order)"
9a calls get "$CALL_ID"
9a calls events "$CALL_ID" --after 0 --limit 100
```

Persistent tracking does not remove the adapter or upstream task lifetime; any
timeout enforced by that adapter still applies. Only the call owner or an
administrator can read the record and events.
Cancellation requires a capability that declares `cancelable` and an active
adapter invocation:

```sh
9a calls cancel <active-cancelable-call-id>
```

Terminal records and events survive restart. During a clean shutdown, an active
call is completed as `failed` with code `app_closed`. After a crash or another
stop that leaves an active record persisted, restore completes that record as
`failed` with code `daemon_restarted`; calls are not automatically resumed.

## Command reference

Run `9a --help` for the grouped command overview and
`9a help <command> [subcommand]` for complete positional arguments, flags, and
examples. `9a completion <shell>` generates completion for bash, zsh, fish, or
powershell. Commands print concise human-readable output by default. Add the
global `--json` flag to any data command for stable machine-readable output;
successful commands without response data return `{"ok":true}`. Token creation
and asynchronous call start keep their useful plain scalar output by default.

| Command | Purpose | Required access |
| --- | --- | --- |
| `9a validate <source.yaml>` | Strictly validate a declarative Skill without contacting the daemon | local file access |
| `9a add <source.yaml>` | Add or update a declarative API Skill in the current workspace | `admin` |
| `9a diff <source.yaml>` | Compare a YAML source with its persisted version | `admin` |
| `9a remove <skill-name>` | Remove a declarative source and its owned projection | `admin` |
| `9a adapters add <protocol> <absolute-executable>` | Persistently register an executable adapter | `admin` |
| `9a providers add <protocol> <name> <endpoint>` | Discover and persist a provider | `admin` |
| `9a providers remove <protocol> <name>` | Remove a provider and its managed views | `admin` |
| `9a attach [--workspace PATH] [--backend auto\|fuse\|directory]` | Attach the built-in Skill and workspace view | authenticated identity |
| `9a status [--workspace PATH]` | Inspect backend, fallback, and managed Skills | authenticated identity |
| `9a update [--workspace PATH] [--check\|--all]` | Rediscover providers and reconcile managed views | `admin` |
| `9a detach [--workspace PATH]` | Remove only this workspace's managed view | authenticated identity |
| `9a tokens create <identity>` | Create a bearer token for an identity | `admin` |
| `9a acl grant <identity> <capability> <permissions>` | Grant comma-separated `read`, `invoke`, `write`, or `admin` permissions | `admin` |
| `9a search <query...>` | Search visible capabilities; unquoted words are joined | capability `read` |
| `9a project add <capability> <skills-root>` | Materialize one filesystem Skill | capability `read` |
| `9a invoke <capability>` | Read up to 8 MiB of JSON and wait with a 30-second CLI timeout | capability `invoke` |
| `9a calls start <capability>` | Persist up to 8 MiB of JSON and start an asynchronous call | capability `invoke` |
| `9a calls get <call-id>` | Read call state and terminal result | call owner or `admin` |
| `9a calls events <call-id> [--after N] [--limit N]` | Read a persistent event page | call owner or `admin` |
| `9a calls cancel <call-id>` | Request confirmed cancellation | call owner or `admin` |
| `9a completion <bash\|zsh\|fish\|powershell>` | Generate shell completion on stdout | local execution |
| `9a version` or `9a --version` | Print the embedded client version | local execution |

## Common failures

- **Daemon did not start:** inspect
  `$HOME/.local/state/ninea/daemon.log`; `NINEA_SOCKET` must match any explicit
  service configuration.
- **`unauthorized`:** set `NINEA_TOKEN` to a token issued for this identity.
- **Bootstrap failure on restart:** unset `NINEA_BOOTSTRAP_TOKEN` after the
  first successful start.
- **Empty search:** grant `read` on the capability to that identity.
- **`permission_denied`:** projection needs `read`; execution needs `invoke`;
  adapter, provider, token, and ACL mutation needs `admin`.
- **Provider discovery failure:** confirm the endpoint, daemon-inherited
  provider credential, protocol version, and adapter diagnostics.
- **Projection conflict:** NineA refuses to replace any path it does not own.
- **FUSE fallback:** inspect `9a status --json`; enable the platform FUSE
  runtime or require the portable directory backend explicitly.
- **Tampered projection:** run `9a update`; edit the source YAML or provider,
  never the generated Skill. Move a directory with a missing or corrupt
  ownership manifest aside before updating; NineA will not delete it.
- **Call cannot be canceled:** the capability may be non-cancelable, already
  terminal, or no longer active in this daemon process.
- **`call_quota_exceeded: call quota exceeded`:** the identity or daemon has
  reached an active-call, retained-call, or retained-bytes limit. Wait for
  active calls to finish; retained records require offline archival or a new
  state database because the current 0.x release has no online call deletion or
  GC.
