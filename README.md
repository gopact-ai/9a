# NineA

NineA is a local capability runtime for AI agents. It turns integrations into
capabilities that a human or agent can search and run from the current project.

## Install

```sh
brew install gopact-ai/tap/ninea
```

Already installed? Update with `brew upgrade gopact-ai/tap/ninea`.

## 60-second quick start

In any project, save this as `weather.yaml`. If an agent is translating API
documentation, it can read the embedded contract before the first integration
or gateway Skill exists:

```sh
9a connect --guide http --json
```

Then create:

```yaml
version: 1
name: weather
description: Read current weather for coordinates.
type: http

services:
  forecast:
    baseURL: https://api.open-meteo.com

capabilities:
  current-weather:
    description: Read current temperature and wind.
    service: forecast
    method: GET
    path: /v1/forecast
    request:
      query:
        latitude: "{{ input.latitude }}"
        longitude: "{{ input.longitude }}"
        current: temperature_2m,wind_speed_10m
    inputSchema:
      type: object
      required: [latitude, longitude]
      additionalProperties: false
      properties:
        latitude: {type: number}
        longitude: {type: number}
    outputSchema:
      type: object
      required: [temperature_2m, wind_speed_10m]
      properties:
        temperature_2m: {type: number}
        wind_speed_10m: {type: number}
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body.current
```

Then run:

```sh
9a connect weather.yaml
9a search weather/current-weather --json
9a run weather/current-weather \
  --input '{"latitude":31.2,"longitude":121.5}'
```

`connect` prints `9a search <integration> --json` to list every connected
capability. Use an exact `<integration>/<capability>` search from that result to
inspect its input and output contracts. NineA saves the editable source at
`.9a/integrations/weather.yaml`; re-running it updates the same integration.
Version 1 HTTP manifests require an explicit `inputSchema` and `outputSchema`
for every capability and workflow, so agents never have to infer a contract
from request templates.

## Three concepts

- **Integration** — one external system, such as a weather API.
- **Capability** — one action, such as `current-weather`.
- **Workspace** — the project whose agent can use those capabilities.

NineA prepares a workspace automatically and installs one `using-ninea`
gateway Skill shared by every integration.

## Daily use

```sh
9a search weather
9a run weather/current-weather --input @request.json
cat request.json | 9a run weather/current-weather
9a status
9a doctor
9a disconnect weather
```

`status` reports whether integrations are ready and prints the next action for
missing credentials or broken state. `disconnect` removes the active
integration but keeps `.9a/integrations/weather.yaml`. Add `--json` to data
commands for machine-readable output.

Capabilities that may change an upstream system require explicit approval:

```sh
9a run orders/create-order --input @order.json --json
# After the user approves this exact input, reuse data.approvalToken:
9a run orders/create-order --input @order.json \
  --approve "$APPROVAL_TOKEN"
```

The approval token binds the capability revision and exact JSON input. It can
be used once, expires after 10 minutes, and is invalidated when the local
daemon restarts. Changing either bound value also requires a new preflight and
explicit approval.

## Authenticated APIs

Manifests name credential aliases, never secret values. Store a declared value
through stdin or the hidden terminal prompt:

```sh
9a secret set private-api.api-token
printf '%s' "$API_TOKEN" | 9a secret set private-api.api-token
```

Values go to the operating system credential store (system keyring) and are
scoped to the current workspace. The same reference can hold a different value
in another workspace. See the
[authenticated HTTP example](examples/integrations/authenticated-http.yaml).

## Other integration types

Connect a local MCP server or a remote A2A agent without writing YAML:

```sh
9a connect mcp --name local-tools -- /absolute/path/to/server
9a connect a2a --name research-agent https://agent.example.com
```

For a bearer-authenticated A2A agent, use an
[A2A manifest](docs/reference/manifest.md#a2a) and store its declared credential
with `9a secret set`.

## Use from an agent

The `using-ninea` gateway Skill teaches an agent to search for capabilities and
call `9a run`. Capability descriptions are loaded only when needed instead of
being added to the prompt at startup.

The manifest is an Agent-to-NineA contract, not a form the user must learn.
Give an agent API documentation or an OpenAPI file and ask it to create an
integration and connect it. On a fresh workspace it starts with
`9a connect --guide http --json`; successful connection then projects the shared
gateway Skill.

## Trust boundary

Searching and checking status have no upstream side effect. During normal
capability use, `run` is the execution boundary, and mutating actions require
an approval token. All MCP and A2A runs require approval. Integration YAML is
strictly decoded: unknown fields, duplicate keys, aliases, multiple documents,
unsafe remote HTTP URLs, and oversized input are rejected.

MCP servers and executable hooks are local code running with your operating
system account's privileges. Use a sandbox or separate account when that trust
boundary is too broad. See [Security](docs/SECURITY.md).

See the [documentation index](docs/INDEX.md) for guides and reference material.

## Development

```sh
go test -count=1 ./...
```

NineA is available under the [MIT License](LICENSE).
