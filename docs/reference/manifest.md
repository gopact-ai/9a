# Integration manifest reference

Every version 1 manifest starts with a common envelope:

```yaml
version: 1
name: weather
description: Read current weather.
type: http
```

`version`, `name`, and `type` are required. `description` is optional. Names use
lowercase kebab-case. `type` is `http`, `mcp`, or `a2a`.

## HTTP

An HTTP manifest declares at least one service and capability:

```yaml
version: 1
name: weather
description: Read current weather.
type: http

services:
  forecast:
    baseURL: https://api.example.com
    timeout: 30s

capabilities:
  current:
    description: Read current weather.
    service: forecast
    method: GET
    path: /v1/weather
    request:
      query:
        city: "{{ input.city }}"
    inputSchema:
      type: object
      required: [city]
      additionalProperties: false
      properties:
        city:
          type: string
    outputSchema:
      type: object
      required: [temperature]
      properties:
        temperature:
          type: number
```

Service, capability, workflow, and step names also use lowercase kebab-case.
Each capability selects a service, HTTP method, and root-relative path. Methods
are `GET`, `POST`, `PUT`, `PATCH`, and `DELETE`.

Request query, headers, and JSON body may contain `{{ input.path }}` templates.
An exact template preserves its JSON type; a template embedded in text becomes
a string.

Every capability and workflow must explicitly declare both `inputSchema` and
`outputSchema`. They use JSON Schema. Use `{}` only when accepting any JSON is
intentional; omitting a schema or setting it to `null` is an error. NineA
compiles both schemas when connecting, validates input before the upstream
request, and validates output before completing the call. External `$ref`
values are not allowed.

Services may use HTTPS URLs; HTTP is limited to loopback development hosts.
Service headers apply to every capability using that service. `timeout` uses a
Go duration such as `500ms`, `10s`, or `2m`, with a five-minute maximum.

### Credentials

A manifest declares credential aliases and references but never values:

```yaml
credentials:
  api-token:
    secret: private-api.api-token

services:
  api:
    baseURL: https://api.example.com
    headers:
      Authorization: "Bearer {{ secrets.api-token }}"
```

The reference must start with the current integration name. Store its value
after connecting:

```sh
9a secret set private-api.api-token
```

`{{ secrets.<alias> }}` templates may appear wherever request strings are
supported. Values are resolved from the operating system credential store for
the current workspace on each run, so rotation takes effect without
reconnecting. Identical references in different workspaces resolve to
independent values. The local database stores only scoped metadata.

### Hooks and approval

Optional `beforeRequest` hooks can set or remove headers or run a bounded `jq`
transform. `afterResponse` hooks can transform the response. Each hook item
contains exactly one action. Structural `jq` transforms preserve arbitrary-size
integers and precision-sensitive decimals. Numeric operations on a decimal that
cannot be represented exactly as `float64` fail instead of rounding silently.

An executable hook requires `security.allowExecutableHooks: true`, an absolute
executable path, and an explicit environment-variable allowlist. Capabilities
with executable hooks require an approval token, as do all HTTP methods other
than GET. A GET that charges, sends, starts work, or otherwise changes state
must declare `requiresApproval: true` beside `method`; this can add approval but
cannot disable the mandatory cases. `--approve <token>` takes the token returned
by the unchanged run's `approval_required` preflight.

### Workflows

Sequential workflows may pass values from `input` and earlier `steps`:

```yaml
workflows:
  city-weather:
    inputSchema:
      type: object
      required: [city]
      additionalProperties: false
      properties:
        city: {type: string}
    outputSchema:
      type: object
      required: [temperature]
      properties:
        temperature: {type: number}
    steps:
      - id: location
        use: find-location
        input:
          city: "{{ input.city }}"
      - id: forecast
        use: current-weather
        input:
          latitude: "{{ steps.location.latitude }}"
          longitude: "{{ steps.location.longitude }}"
```

Workflow steps call capabilities from the same integration. They run in order
and can reference only earlier results. A workflow is exposed through the same
`<integration>/<name>` run interface. It requires approval if any step does.

## MCP

An MCP integration starts one absolute executable without arguments:

```yaml
version: 1
name: local-tools
type: mcp
executable: /absolute/path/to/server
```

The equivalent shortcut is:

```sh
9a connect mcp --name local-tools -- /absolute/path/to/server
```

The executable runs with the current operating-system account's permissions.
NineA supplies only a small process environment needed for executable lookup,
temporary files, locale, and TLS certificates; it does not forward cloud,
source-control, or NineA credentials. MCP manifests cannot inject additional
environment variables or declare NineA credentials. Every MCP tool requires an
approval preflight and token, including tools that advertise `readOnlyHint`.
NineA treats that annotation as untrusted descriptive metadata because the
executable at a path can change. The executable's working directory is the
owning workspace.

## A2A

An A2A integration points to an agent endpoint:

```yaml
version: 1
name: research-agent
type: a2a
url: https://agent.example.com
```

If the Agent Card selects bearer authentication, declare exactly one
credential:

```yaml
version: 1
name: research-agent
type: a2a
url: https://agent.example.com
credentials:
  bearer:
    secret: research-agent.bearer
```

Connect the manifest, then store the workspace-scoped value:

```sh
9a connect research-agent.yaml
9a secret set research-agent.bearer
```

The equivalent shortcut is:

```sh
9a connect a2a --name research-agent https://agent.example.com
```

Remote endpoints require HTTPS. Loopback HTTP is accepted for local
development. The shortcut is for agents that do not require bearer
authentication. Every A2A capability requires approval before execution.

Exact search exposes the A2A invocation envelope as JSON Schema. `parts` is
required and contains one or more objects. Each part must contain exactly one
of `text`, standard-base64 `raw`, absolute `url`, or arbitrary JSON `data`.
Optional part fields are `metadata`, `filename`, and `mediaType`. The optional
top-level `configuration` accepts only `acceptedOutputModes` and
`historyLength`; NineA controls `returnImmediately` itself. Unknown fields are
rejected before a call is created.

## Decoder limits

The decoder rejects unknown fields, duplicate keys, YAML aliases, multiple
documents, malformed templates, invalid references, and sources larger than
8 MiB. Secret values must not appear in a manifest.
