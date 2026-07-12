# NineA

[简体中文](docs/zh-CN/README.md)

## Turn APIs, MCP tools, and A2A agents into Skills every agent can use

NineA is a capability layer for AI agents. It turns heterogeneous upstream
systems into inspectable, executable Skills on the local filesystem—the
interface coding agents already understand best.

```text
YAML APIs ─┐
MCP tools  ├──→ NineA Catalog ──→ filesystem Skills ──→ any agent
A2A agents ┘          │                    │
                      └── local search      └── explicit commands
```

The fastest path is one YAML file:

```yaml
apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: weather
  description: Find a city and read its current weather.
services:
  forecast:
    baseURL: https://api.open-meteo.com
operations:
  current-weather:
    service: forecast
    method: GET
    path: /v1/forecast
    request:
      query:
        latitude: "{{ input.latitude }}"
        longitude: "{{ input.longitude }}"
        current: temperature_2m
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body.current
```

```sh
9a validate weather.yaml
9a add weather.yaml
printf '%s\n' '{"latitude":31.2,"longitude":121.5}' | \
  .agents/skills/weather/operations/current-weather/invoke
```

NineA materializes a real Skill, not a hidden tool registration:

```text
.agents/skills/weather/
├── SKILL.md
├── operations/current-weather/
│   ├── schema.json
│   └── invoke
└── references/source.yaml
```

Add more operations, more API origins, or ordered workflows to the same YAML
and they remain one coherent domain Skill. Variables resolve from the daemon
environment. Request and response hooks can set headers, remove headers, or
shape JSON with embedded jq. An explicitly enabled, bounded executable hook is
available for signing and transformations that cannot be declarative.

See the runnable [Open-Meteo example](examples/declarative/open-meteo.yaml),
the [multi-API bundle](examples/declarative/api-bundle.yaml), and the complete
[Declarative Skills manual](docs/declarative-skills.md).

## Why files and commands

AI agents are already excellent at two durable interfaces:

- the **filesystem** for discovery and context—plain instructions, schemas,
  provenance, search, and selective loading;
- the **command line** for execution—a visible boundary between reading about
  a capability and causing an upstream side effect.

NineA keeps thousands of capabilities out of the prompt. Agents search the
local Catalog, load only the useful Skills into their namespace, and invoke a
small command with JSON. The consuming agent does not need an MCP client, A2A
client, API SDK, credential handling, or a vendor-specific tool registry.

This shape takes inspiration from Plan 9 namespaces: heterogeneous resources
become easier to compose when adapters present a small common interface and
each caller assembles the view it needs. NineA does not implement 9P and does
not pretend remote actions are files. Files disclose capabilities; commands
perform actions. Read [Architecture and Plan 9](docs/architecture.md).

## Install

Homebrew installs the `9a` client and `ninead` daemon on macOS or Linux:

```sh
brew install gopact-ai/tap/ninea
```

[GitHub Releases](https://github.com/gopact-ai/9a/releases) provides archives
and SHA-256 checksums for macOS and Linux on x86-64 and ARM64.

On the first daemon start, create an administrator token:

```sh
export NINEA_SOCKET="$HOME/.local/state/ninea/ninea.sock"
export NINEA_TOKEN="$(openssl rand -hex 32)"
mkdir -p "$(dirname "$NINEA_SOCKET")"
NINEA_BOOTSTRAP_TOKEN="$NINEA_TOKEN" ninead \
  --state "$HOME/.local/state/ninea/ninea.db" \
  --socket "$NINEA_SOCKET"
```

Leave `NINEA_BOOTSTRAP_TOKEN` unset on later starts. The
[Getting Started guide](docs/getting-started.md) covers persistent startup,
separate agent identities, ACLs, MCP, A2A, and the complete command reference.

## Three integration paths

| Upstream | Integration path | Best for |
| --- | --- | --- |
| JSON HTTP APIs | Built-in declarative YAML | One API or a domain bundle, environment variables, hooks, and workflows |
| MCP | Built-in local stdio adapter | Existing MCP servers and tool discovery |
| A2A | Built-in HTTP+JSON 1.0 adapter | Existing agents, skills, asynchronous Tasks, and cancellation |
| Any other protocol | Language-neutral `9a.adapter/v1` executable | Custom discovery, streaming semantics, retries, or non-HTTP transports |

MCP and A2A capabilities enter the same Catalog and can be projected
selectively:

```sh
9a search "weather temperature"
9a project add mcp/weather/get-weather .agents/skills
printf '%s\n' '{"location":"Shanghai"}' | \
  .agents/skills/ninea-mcp-weather-get-weather/scripts/invoke
```

For work that must outlive one CLI request, NineA persists call state, results,
events, and confirmed cancellation in SQLite:

```sh
CALL_ID="$(printf '%s\n' '{"location":"Shanghai"}' | \
  9a calls start mcp/weather/get-weather)"
9a calls get "$CALL_ID"
9a calls events "$CALL_ID" --limit 100
```

## Security boundaries

NineA uses bearer identities, a private Unix socket, and default-deny
capability ACLs. Reading and invocation are separate permissions. Remote API
URLs require HTTPS, except loopback development endpoints. YAML is strictly
decoded before installation; secrets remain environment references.

Executable hooks, MCP servers, and custom executable adapters are trusted local
code running with the daemon user's privileges. Use a dedicated OS account or
sandbox when that boundary is not strong enough. Read the complete
[Security guide](docs/SECURITY.md).

## Documentation

- [Declarative Skills](docs/declarative-skills.md)—YAML schema, variables,
  templates, hooks, workflows, lifecycle, and troubleshooting
- [Declarative examples](examples/declarative/README.md)—public weather,
  authenticated APIs, multi-API bundles, and executable hooks
- [Getting Started](docs/getting-started.md)—daemon, identities, MCP, A2A, and
  CLI reference
- [Building adapters](docs/adapters.md)—custom executable protocol and registry
- [Architecture and Plan 9](docs/architecture.md)
- [Security](docs/SECURITY.md)
- [Contributing](docs/CONTRIBUTING.md)

Run the complete test suite, including process-level E2E coverage:

```sh
go test -count=1 ./...
```

NineA is available under the [MIT License](LICENSE).
