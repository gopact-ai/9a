# Declarative Skills

Declarative Skills are the shortest path from ordinary JSON APIs to an
agent-ready filesystem interface. Describe one domain in YAML, run `9a add`,
and NineA creates one Skill containing every declared operation and workflow.
No adapter project, SDK, or generated client is required.

## 🚀 Quick start

Start `ninead` as described in the [User Guide](getting-started.md), then run:

```sh
9a validate examples/declarative/open-meteo.yaml
9a add examples/declarative/open-meteo.yaml

printf '%s\n' '{"city":"Shanghai"}' | \
  .agents/skills/weather/workflows/city-weather/invoke
```

`validate` is local and does not require the daemon. `add`, `diff`, and
`remove` are administrative operations and use the active `NINEA_TOKEN`.

```sh
9a diff examples/declarative/open-meteo.yaml
9a remove weather
```

`add` is also the update command. NineA validates the complete replacement,
publishes through an owned staging directory, updates the Catalog, and keeps
the source in SQLite so the integration survives daemon restart. `diff`
reports added, removed, and modified operations or workflows before an update.

## 🗂️ Generated filesystem interface

The weather example produces:

```text
.agents/skills/weather/
├── SKILL.md
├── operations/
│   ├── current-weather/
│   │   ├── invoke
│   │   └── schema.json
│   └── find-location/
│       ├── invoke
│       └── schema.json
├── workflows/
│   └── city-weather/
│       ├── invoke
│       └── schema.json
└── references/
    └── source.yaml
```

An agent can read `SKILL.md`, inspect only the relevant schema, and invoke a
single command with JSON on stdin. The command returns JSON on stdout. API
credentials remain in the daemon environment; they are not resolved into the
projected source.

## 🧩 Document structure

Every document uses the following envelope:

```yaml
apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: example
  description: What this API domain lets an agent do.
services: {}
operations: {}
```

NineA uses strict YAML decoding. Unknown fields, duplicate keys, aliases,
non-string keys, multiple YAML documents, malformed templates, and documents
larger than 8 MiB are rejected. Names use lowercase words separated by single
hyphens, for example `order-operations`.

### `metadata`

| Field | Required | Meaning |
| --- | --- | --- |
| `name` | Yes | Stable Skill and provider name. |
| `description` | Recommended | Human- and agent-readable domain summary. |

Changing `metadata.name` creates a different Skill. Use a stable domain name,
not a version number or deployment name.

### `projection`

By default, `9a add` writes under `.agents/skills` in the directory where the
command runs. A source can select another relative root:

```yaml
projection:
  targets: [.claude/skills]
```

The current version accepts one relative target. NineA refuses absolute paths,
parent traversal, and replacement of directories it does not own.

### `variables`

Variables separate portable configuration from machine-local values:

```yaml
variables:
  api-token:
    fromEnv: ORDERS_API_TOKEN
    sensitive: true
    required: true
  region:
    fromEnv: ORDERS_REGION
    default: us-east
```

| Field | Default | Meaning |
| --- | --- | --- |
| `fromEnv` | Empty | Environment variable read by `ninead` at invocation time. |
| `default` | Empty | Value used when the environment variable is unset or empty. |
| `required` | `false` | Fail the invocation when neither source provides a value. |
| `sensitive` | `false` | Declares secret intent for tools and future policy checks. |

Do not put secret values in YAML. Start or restart `ninead` from an environment
that contains the variables. Templates refer to the logical YAML name, not the
environment variable name: `{{ vars.api-token }}`.

### `services`

A Skill can combine any number of API origins:

```yaml
services:
  customers:
    baseURL: https://customers.example.com
    timeout: 15s
    headers:
      Authorization: "Bearer {{ vars.api-token }}"
      Accept: application/json
  local-development:
    baseURL: http://127.0.0.1:8080
```

Remote services require HTTPS. Plain HTTP is accepted only for loopback hosts.
URLs cannot contain credentials, a query, or a fragment. Service headers apply
to every operation using that service. Timeouts use Go duration syntax such as
`500ms`, `10s`, or `2m`, and cannot exceed five minutes.

### `operations`

An operation maps JSON input to one HTTP request:

```yaml
operations:
  create-order:
    description: Create an order.
    service: orders
    method: POST
    path: /v1/orders
    request:
      query:
        dry_run: "{{ input.dryRun }}"
      headers:
        Idempotency-Key: "{{ input.requestId }}"
      body:
        customer_id: "{{ input.customerId }}"
        items: "{{ input.items }}"
    inputSchema:
      type: object
      required: [requestId, customerId, items]
    outputSchema:
      type: object
```

Supported methods are `GET`, `POST`, `PUT`, `PATCH`, and `DELETE`. Paths must
be root-relative and cannot contain a host, query, fragment, or `..` segment.
When `body` is present, NineA encodes it as JSON and supplies
`Content-Type: application/json` unless the configuration overrides it.

`inputSchema` and `outputSchema` are projected for agent inspection. They are
contracts and discovery metadata; this version does not perform full JSON
Schema validation during invocation.

## 🧬 Templates

Templates can appear in service headers, request query values, request headers,
request bodies, hook headers, and workflow step inputs.

| Namespace | Available in | Example |
| --- | --- | --- |
| `input` | Operations and workflows | `{{ input.customer.id }}` |
| `vars` | Operations and workflows | `{{ vars.api-token }}` |
| `steps` | Workflow steps after the referenced step | `{{ steps.customer.id }}` |

An exact template preserves the JSON value's type. If `input.items` is an
array, `items: "{{ input.items }}"` remains an array. A template embedded in
larger text is converted to a string, as in
`Authorization: "Bearer {{ vars.api-token }}"`. Missing values fail before the
network request is sent.

## 🪝 Hooks

Hooks execute in declaration order. Each list item must contain exactly one
action.

### Request hooks

`beforeRequest` sees an object containing `input`, `query`, `headers`, and
`body`. Most integrations need only declarative header actions:

```yaml
hooks:
  beforeRequest:
    - setHeaders:
        X-Tenant: "{{ input.tenant }}"
        X-Client: "{{ vars.client-name }}"
    - removeHeaders: [X-Debug]
```

A jq transform can rewrite the full request object:

```yaml
hooks:
  beforeRequest:
    - transform:
        language: jq
        expression: '.body |= (. + {source: "agent"})'
```

The transform must produce exactly one object with the request fields needed by
the next stage.

### Response hooks

`afterResponse` receives:

```json
{
  "status": 200,
  "headers": {"Content-Type": ["application/json"]},
  "body": {"upstream": "response"}
}
```

Use jq to expose a small, stable result to agents:

```yaml
hooks:
  afterResponse:
    - transform:
        language: jq
        expression: '{id: .body.id, state: .body.status}'
```

NineA embeds the jq interpreter; no external `jq` binary is required. A
transform must produce exactly one JSON value. Non-2xx responses fail before
response hooks run.

### Executable hook escape hatch

Use executable hooks only when header and jq actions cannot express signing,
legacy encodings, or a specialized transformation. They are disabled unless
the document opts in:

```yaml
security:
  allowExecutableHooks: true
```

```yaml
- exec:
    command: [/absolute/path/to/sign-request, --format, v2]
    env: [ORDER_SIGNING_SECRET]
    timeout: 2s
    maxOutputBytes: 1048576
```

The program reads one JSON value from stdin and must write exactly one JSON
value to stdout. Request hooks must return an object. NineA executes the command
array directly without shell interpolation, passes only the listed environment
variables, starts a separate process group, kills the group on cancellation,
timeout, or output overflow, limits stderr, and caps the timeout at 30 seconds
and output at 8 MiB. A global admission limit allows at most 32 executable hooks
at once; additional hooks fail without starting a process.

Executable hooks run with the daemon user's operating-system privileges. Review
the executable and prefer a separate OS sandbox when it handles untrusted data.
See [`executable-hook.yaml`](../examples/declarative/executable-hook.yaml) for a
complete example.

## 🔄 Workflows

Workflows compose operations from the same Skill without introducing another
service:

```yaml
workflows:
  customer-orders:
    steps:
      - id: customer
        use: find-customer
        input:
          email: "{{ input.email }}"
      - id: orders
        use: list-orders
        input:
          customerId: "{{ steps.customer.id }}"
    output:
      language: jq
      expression: '{customer: .steps.customer, orders: .steps.orders}'
```

Steps run sequentially and may reference only prior step results. `use` names a
declared operation. Cancellation or timeout propagates through the active HTTP
request. The final jq expression receives `{input, steps}`. Without `output`,
NineA returns that complete object.

Workflows are deliberately bounded and deterministic in this version: no
parallel branches, loops, conditions, retries, or workflow-to-workflow calls.
Put retry policy at the API gateway or implement a reviewed custom adapter when
those semantics are essential.

## ♻️ Lifecycle and update behavior

| Command | Daemon required | Effect |
| --- | --- | --- |
| `9a validate <file>` | No | Strictly parse and list capability ids. |
| `9a add <file>` | Yes | Add or replace the source, Catalog entries, and owned Skill. |
| `9a diff <file>` | Yes | Compare against the persisted source without changing it. |
| `9a remove <name>` | Yes | Remove Catalog entries and the owned Skill directory. |
| `9a update` | Yes | Rediscover providers and repair or upgrade every managed Skill in the workspace. |
| `9a detach` | Yes | Remove the workspace view without deleting this persisted source. |

`add` grants the administrator performing the import `read` and `invoke` for
the generated capabilities. Create separate agent identities and grant only
the operations they need when agents should not use the administrator token.

```sh
AGENT_TOKEN="$(9a tokens create support-agent)"
9a acl grant support-agent api/order-operations/customer-orders read,invoke
```

The projected ownership manifest prevents overwriting user-created directories
and records modes and SHA-256 digests without duplicating file contents.
If a source changes its projection target, a successful update removes the old
owned projection. Source text and projection location are stored in SQLite and
reloaded with the built-in API adapter after restart.

Managed Skills are read-only. Change the original YAML and run `9a add` again.
With FUSE, mutation is rejected by the filesystem. The directory fallback uses
`0444`/`0555` modes, atomic replacement, integrity checks, and `9a update`
repair; it is not a security boundary against the same OS account.

## 🛠️ Troubleshooting

- **Required environment variable is missing:** place it in the environment
  used to start `ninead`, then restart the daemon.
- **Remote base URL must use HTTPS:** use HTTPS, or a loopback URL for local
  development.
- **Projection conflict:** move or rename the existing user-owned directory;
  NineA will not overwrite it.
- **Template value is missing:** verify the JSON path and send an exact JSON
  type rather than a formatted string.
- **jq produced no or multiple results:** make the expression return one value;
  array construction is useful when several matches should be one result.
- **Executable hook failed:** run the command manually with representative JSON,
  confirm its absolute path, allowed environment list, timeout, and output cap.

Use [`open-meteo.yaml`](../examples/declarative/open-meteo.yaml) for a runnable
public API, [`authenticated-api.yaml`](../examples/declarative/authenticated-api.yaml)
for credentials and request mapping, and
[`api-bundle.yaml`](../examples/declarative/api-bundle.yaml) for a multi-API
domain Skill.
