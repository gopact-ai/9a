# NineA

[简体中文](docs/zh-CN/README.md)

## Any capability as a Skill — for every agent

NineA turns MCP tools, A2A agents, and JSON HTTP APIs into filesystem-native
Skills that any agent can discover, inspect, and invoke.

```text
MCP tools ─┐
A2A agents ├─→ adapters → Catalog → filesystem Skills → any agent
HTTP APIs ─┘                    │
                               └─→ authorized command execution
```

Agents already understand two durable interfaces:

- the **filesystem for discovery and context** — instructions, schemas, and
  provenance are ordinary inspectable files;
- the **command line for execution** — a visible command crosses the boundary
  from passive context to an authorized upstream action.

NineA keeps the full capability Catalog out of the prompt. An agent searches
locally, projects only the useful capabilities into its own Skill directory,
then invokes them through a small generated script. The agent does not need an
MCP client, an A2A client, API credentials, or a vendor-specific tool registry.

## Install

Homebrew installs the `9a` client and the `ninead` daemon on macOS or Linux:

```sh
brew install gopact-ai/tap/9a
```

Release archives and SHA-256 checksums are also available from
[GitHub Releases](https://github.com/gopact-ai/9a/releases). NineA currently
publishes binaries for macOS and Linux on x86-64 and ARM64.

## From capability to Skill

```sh
9a search "weather temperature"
9a project add mcp/weather/get-weather .agents/skills
printf '%s\n' '{"location":"Shanghai"}' | \
  .agents/skills/ninea-mcp-weather-get-weather/scripts/invoke
```

A projected Skill contains `SKILL.md`, `schema.json`, bounded upstream
provenance, and `scripts/invoke`. Search and file reads are local and have no
provider side effects. Invocation is separately protected by the capability's
`invoke` permission.

For work that should outlive one CLI request, start a persistent call:

```sh
CALL_ID="$(printf '%s\n' '{"location":"Shanghai"}' | \
  9a calls start mcp/weather/get-weather)"
9a calls get "$CALL_ID"
9a calls events "$CALL_ID" --limit 100
```

Call state, results, and paginated events are stored in SQLite. Cancellation is
available only when the capability declares it and the adapter can confirm it.

## What is available

| Integration | Current alpha |
| --- | --- |
| MCP tools | Built-in local stdio adapter |
| A2A agents | Built-in HTTP+JSON 1.0 adapter for single-turn skills and asynchronous Tasks |
| JSON HTTP APIs | Manifest-driven [generic HTTP adapter](examples/http-adapter/README.md) |
| Custom protocols | Persistent registry for language-neutral `9a.adapter/v1` executables |
| Agent interface | Local search, selective filesystem Skill projection, sync and persistent async execution |
| Access control | Bearer identities, default-deny capability ACLs, separate `read` and `invoke` permissions |

The executable adapter wire contract and registration flow are documented in
[Building adapters](docs/adapters.md).

## Why this shape

Without a shared capability layer, every agent must integrate every upstream
protocol: an N × M integration problem. With NineA, an upstream system uses one
adapter, an agent consumes one Skill format, and NineA owns normalization,
local search, authorization, persistence, and routing.

The design is inspired by Plan 9's namespace model: heterogeneous resources
become easier to combine when they are presented through a small interface and
assembled into a caller-selected namespace. NineA does not implement 9P and
does not pretend remote actions are files. Files disclose capabilities;
commands perform actions. See [Architecture and Plan 9](docs/architecture.md).

## Current boundaries

NineA is an alpha for local evaluation. It currently requires Unix domain
sockets and does not provide provider sandboxing, HTTP MCP transport,
streaming, multi-turn continuation, or a stable compatibility guarantee. MCP
servers and executable adapters are trusted local processes running with the
daemon user's OS privileges.

## Start here

- [Getting started](docs/getting-started.md) — copy-and-run MCP walkthrough,
  authentication, integrations, and complete CLI reference
- [Building adapters](docs/adapters.md) — executable protocol and registry
- [Generic HTTP adapter](examples/http-adapter/README.md) — connect a JSON API
  by editing a manifest
- [Architecture and Plan 9](docs/architecture.md)
- [Security](docs/SECURITY.md)
- [Contributing](docs/CONTRIBUTING.md)

Run the process-level integration suite with:

```sh
go test -count=1 ./test/e2e
```

NineA is available under the [MIT License](LICENSE).
