# Getting Started

Install the `9a` client and `ninead` daemon on macOS or Linux with Homebrew:

```sh
brew install gopact-ai/tap/ninea
```

Building from source requires Go 1.25.12 or newer. NineA currently requires a
platform with Unix domain sockets. The examples below use `openssl` to generate
a bootstrap token.

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
go build -o "$DEMO_DIR/bin/ninead" ./cmd/ninead
go build -o "$DEMO_DIR/bin/mcpfixture" ./testdata/mcpserver

export PATH="$DEMO_DIR/bin:$PATH"
export NINEA_SOCKET="$DEMO_DIR/ninea.sock"
export NINEA_TOKEN="$(openssl rand -hex 32)"

NINEA_BOOTSTRAP_TOKEN="$NINEA_TOKEN" ninead \
  --state "$DEMO_DIR/state.db" \
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

9a search "weather temperature" | grep -o 'mcp/weather/get-weather'
9a project add mcp/weather/get-weather "$DEMO_DIR/skills"
find "$DEMO_DIR/skills/ninea-mcp-weather-get-weather" -maxdepth 2 -type f
printf '%s\n' '{"location":"Shanghai"}' | \
  "$DEMO_DIR/skills/ninea-mcp-weather-get-weather/scripts/invoke"
)
```

## Run a persistent daemon

Create a private state directory and a bootstrap token. The bootstrap token is
accepted only when the database has no tokens. Every later daemon start must
leave `NINEA_BOOTSTRAP_TOKEN` unset.

```sh
mkdir -p "$HOME/.local/state/ninea"
chmod 700 "$HOME/.local/state/ninea"
umask 077
export NINEA_SOCKET="$HOME/.local/state/ninea/ninea.sock"
ADMIN_TOKEN_FILE="$HOME/.local/state/ninea/admin-token"
test -s "$ADMIN_TOKEN_FILE" || openssl rand -hex 32 >"$ADMIN_TOKEN_FILE"
export NINEA_TOKEN="$(cat "$ADMIN_TOKEN_FILE")"

NINEA_BOOTSTRAP_TOKEN="$NINEA_TOKEN" ninead \
  --state "$HOME/.local/state/ninea/ninea.db" \
  --socket "$NINEA_SOCKET" \
  >"$HOME/.local/state/ninea/ninead.log" 2>&1 &
echo $! >"$HOME/.local/state/ninea/ninead.pid"
```

`NINEA_TOKEN` is a client credential used by `9a`; `ninead` removes it and the
bootstrap token from its own environment after startup. Provider credentials
are different: they normally must be present in the `ninead` environment so an
MCP server or executable adapter can inherit them when its child process
starts, unless the integration uses an external secret store.

If an integration below needs environment variables, export them before the
`ninead` command. If the daemon is already running, stop it cleanly and restart
it from the updated environment without `NINEA_BOOTSTRAP_TOKEN`.

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
`research-agent`, start `ninead` with:

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
# Restart ninead from this environment, then:
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

| Command | Purpose | Required access |
| --- | --- | --- |
| `9a validate <source.yaml>` | Strictly validate a declarative Skill without contacting the daemon | local file access |
| `9a add <source.yaml>` | Add or update a declarative API Skill in the current workspace | `admin` |
| `9a diff <source.yaml>` | Compare a YAML source with its persisted version | `admin` |
| `9a remove <skill-name>` | Remove a declarative source and its owned projection | `admin` |
| `9a adapters add <protocol> <absolute-executable>` | Persistently register an executable adapter | `admin` |
| `9a providers add <protocol> <name> <endpoint>` | Discover and persist a provider | `admin` |
| `9a tokens create <identity>` | Create a bearer token for an identity | `admin` |
| `9a acl grant <identity> <capability> <permissions>` | Grant comma-separated permissions | `admin` |
| `9a search <query>` | Search visible capabilities as JSON | capability `read` |
| `9a project add <capability> <skills-root>` | Materialize one filesystem Skill | capability `read` |
| `9a invoke <capability>` | Read up to 8 MiB of JSON and wait with a 30-second CLI timeout | capability `invoke` |
| `9a calls start <capability>` | Persist up to 8 MiB of JSON and start an asynchronous call | capability `invoke` |
| `9a calls get <call-id>` | Read call state and terminal result | call owner or `admin` |
| `9a calls events <call-id> [--after N] [--limit N]` | Read a persistent event page | call owner or `admin` |
| `9a calls cancel <call-id>` | Request confirmed cancellation | call owner or `admin` |

## Common failures

- **Cannot connect:** confirm `ninead` is running and `NINEA_SOCKET` matches.
- **`unauthorized`:** set `NINEA_TOKEN` to a token issued for this identity.
- **Bootstrap failure on restart:** unset `NINEA_BOOTSTRAP_TOKEN` after the
  first successful start.
- **Empty search:** grant `read` on the capability to that identity.
- **`permission_denied`:** projection needs `read`; execution needs `invoke`;
  adapter, provider, token, and ACL mutation needs `admin`.
- **Provider discovery failure:** confirm the endpoint, daemon-inherited
  provider credential, protocol version, and adapter diagnostics.
- **Projection conflict:** NineA refuses to replace any path it does not own.
- **Call cannot be canceled:** the capability may be non-cancelable, already
  terminal, or no longer active in this daemon process.
- **`call_quota_exceeded: call quota exceeded`:** the identity or daemon has
  reached an active-call, retained-call, or retained-bytes limit. Wait for
  active calls to finish; retained records require offline archival or a new
  state database because the current alpha has no online call deletion or GC.
