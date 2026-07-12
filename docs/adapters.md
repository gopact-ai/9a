# Building Adapters

Adapters connect upstream capabilities to NineA. MCP tools, A2A agents, REST
APIs, local commands, and private services all use the same conceptual
boundary: discover capabilities, then invoke a selected capability with
structured input.

> **Availability:** runtime registration of executable adapters is available
> in the current alpha. Version `9a.adapter/v1` is single-turn; it does not
> support streaming or multi-turn continuation.

## Why executable adapters

An adapter is a separate executable instead of a Go package linked into
`ninead`. This provides three useful properties:

- use Python, Go, Rust, JavaScript, or any language that can read and write
  JSON Lines;
- install or update an integration without rebuilding NineA;
- isolate protocol libraries and child-process failures from the daemon
  process. Provider credentials normally remain in the `ninead` environment
  and are inherited by the child unless the adapter uses an external secret
  store.

The adapter communicates over stdin and stdout. Each line is one UTF-8 JSON
message terminated by `\n`. Logs belong on stderr; stdout is reserved for
protocol messages. A request or response line may be at most 8 MiB including
its newline.

## Registry and process model

Only an administrator can register an executable adapter:

```sh
9a adapters add <protocol> /absolute/path/to/executable
```

The protocol must be a canonical slug and cannot be the reserved built-in
protocol `mcp` or `a2a`. NineA resolves symlinks and requires an executable
regular file. The canonical path is stored in SQLite and validated again when
the daemon restores its registry. Providers can be added after registration:

```sh
9a providers add <protocol> <provider-name> <endpoint>
```

Each provider gets a long-lived adapter process on first use. The process may
serve concurrent request IDs, and responses for different IDs may interleave.
NineA stops adapter processes and their descendants during provider close or
daemon shutdown. Registered adapters, providers, and Catalog entries persist;
the executable file must remain available at its registered path after a
restart.

## Adapter lifecycle

An adapter implements three required operations and one conditional operation:

| Operation | Purpose |
| --- | --- |
| `discover` | Return the capabilities exposed by a provider |
| `invoke` | Execute one capability with structured input |
| `health` | Report whether the provider is usable |
| `cancel` | Cancel an invocation; required when a discovered capability declares `cancelable` |

The process may remain alive for multiple requests. Every response repeats the
request `id`. For `invoke`, that ID is also the invocation handle used by
`cancel`.

## Wire format

Every request carries a protocol version, request ID, method, and parameters:

```json
{"version":"9a.adapter/v1","id":"req-1","method":"health","params":{"provider":{"name":"billing","endpoint":"https://billing.example.com"}}}
```

A successful response contains `result`:

```json
{"version":"9a.adapter/v1","id":"req-1","result":{"healthy":true}}
```

An unsuccessful terminal response contains a stable error code and a safe
message:

```json
{"version":"9a.adapter/v1","id":"req-1","error":{"code":"upstream_unavailable","message":"billing API returned 503"}}
```

Unknown fields must be ignored. Unknown protocol versions or methods must
return `unsupported_version` or `unsupported_method` rather than being guessed.
An adapter must not dispatch an unterminated final JSON value at EOF. Clean EOF
is valid only when no residual request bytes remain.

### Events, artifacts, and terminal responses

An invocation may emit zero or more events or artifacts before exactly one
terminal `result` or `error`. Events carry a monotonically increasing sequence
number scoped to the invocation:

```json
{"version":"9a.adapter/v1","id":"req-3","event":{"sequence":1,"type":"status","data":{"state":"working"}}}
```

Binary artifact data uses base64 encoding:

```json
{"version":"9a.adapter/v1","id":"req-3","artifact":{"sequence":2,"name":"invoice.pdf","media_type":"application/pdf","encoding":"base64","data":"JVBERi0xLjQK"}}
```

The final message has no sequence number:

```json
{"version":"9a.adapter/v1","id":"req-3","result":{"output":{"invoice_id":"inv_456","status":"draft"}}}
```

Messages from different request IDs may be interleaved. Within one invocation,
sequence numbers must increase without duplicates. An adapter must not emit an
event or artifact after that invocation's terminal response.

### Cancellation

NineA cancels an active invocation by sending its request ID as the invocation
handle:

```json
{"version":"9a.adapter/v1","id":"cancel-1","method":"cancel","params":{"invocation_id":"req-3"}}
```

The cancel request ends with `{"canceled":true}` after the adapter has stopped
the upstream work. The original invocation then terminates with a `canceled`
error. If the invocation has already finished or cannot be canceled, the cancel
request returns `not_cancelable` and does not change the original result.

Version `9a.adapter/v1` is single-turn. It can keep one invocation open for
progress events, artifacts, and a terminal result, but it has no operation for
supplying follow-up input to that invocation. NineA must reject a v1 discovery
response that declares `multi_turn: true`.

## Discover capabilities

NineA supplies provider configuration to `discover`:

```json
{"version":"9a.adapter/v1","id":"req-2","method":"discover","params":{"provider":{"name":"billing","endpoint":"https://billing.example.com"}}}
```

The adapter returns upstream-relative descriptions. NineA derives the final
Capability ID and source namespace from the registered protocol and provider:

```json
{"version":"9a.adapter/v1","id":"req-2","result":{"capabilities":[{"upstream_name":"create-invoice","kind":"api.operation","name":"Create invoice","description":"Create a draft invoice for one customer","input":{"mode":"json","json_schema":{"type":"object","required":["customer_id","amount"],"properties":{"customer_id":{"type":"string"},"amount":{"type":"number","minimum":0}}}},"output":{"mode":"json"},"lifecycle":{"sync":true,"streaming":false,"multi_turn":false,"cancelable":false},"security":{"requires_approval":"always","upstream_auth":"adapter-configured"},"tags":["billing","write"]}]}}
```

Descriptions are untrusted upstream data. Keep them factual, bounded, and free
of instructions directed at the consuming agent. The executable adapter
runtime must validate the complete response against this contract before
replacing the provider's Catalog revision.

## Invoke a capability

NineA sends normalized source information and JSON input:

```json
{"version":"9a.adapter/v1","id":"req-3","method":"invoke","params":{"provider":{"name":"billing","endpoint":"https://billing.example.com"},"capability":{"upstream_name":"create-invoice"},"input":{"customer_id":"cus_123","amount":42.5}}}
```

NineA checks that input is bounded, syntactically valid JSON. Input and output
schemas describe the contract exposed to agents, but the current runtime does
not evaluate JSON Schema. An adapter that requires schema enforcement must
perform it before contacting the upstream system.

The adapter returns a structured result:

```json
{"version":"9a.adapter/v1","id":"req-3","result":{"output":{"invoice_id":"inv_456","status":"draft"}}}
```

An adapter must not reinterpret NineA authorization. `ninead` decides whether
the caller may invoke a Capability; the adapter authenticates only to the
upstream system.

## Generic HTTP API adapter

The repository includes a production-oriented, standard-library Go example at
[examples/http-adapter](../examples/http-adapter/README.md). It reads a strict
manifest once at startup, exposes configured JSON HTTP operations, isolates
per-provider bearer credentials, and implements the executable adapter
protocol described above.

Build it, configure its manifest and token environment, then register its
absolute executable path:

```sh
# Set these before starting ninead.
export NINEA_HTTP_ADAPTER_MANIFEST=/absolute/path/to/manifest.json
export NINEA_HTTP_TOKEN_BILLING=replace-with-provider-token
```

After the daemon has started from that environment, use an administrator client
to register the adapter and provider:

```sh
9a adapters add billing-api /absolute/path/to/http-adapter
9a providers add billing-api billing https://billing.example.com
9a search "create invoice"
9a project add billing-api/billing/create-invoice .agents/skills
```

The manifest and token variables must be present in the daemon environment so
the child adapter process inherits them. A provider token variable is derived
from its canonical name: `billing` uses `NINEA_HTTP_TOKEN_BILLING` and
`orders-api` uses `NINEA_HTTP_TOKEN_ORDERS_API`.

The example supports JSON `GET`, `POST`, `PUT`, `PATCH`, and `DELETE`
operations with `none` or `bearer` authentication. Its discovered lifecycle is
synchronous, non-streaming, single-turn, and non-cancelable. The Agent sees a
Skill and an invocation command; it never receives the API token.

## Built-in MCP and A2A adapters

### MCP

The built-in MCP adapter starts a local stdio server, maps `tools/list` to
discovery, and maps `tools/call` to invocation. The current alpha does not
support HTTP MCP transport. The `stdio:` endpoint must name an absolute
executable path; NineA resolves symlinks and runs the canonical executable.
Each invocation has its own process group. Context cancellation, confirmed
call cancellation, provider close, and daemon shutdown terminate that process
and its descendants.

### A2A

The built-in A2A adapter discovers an Agent Card over HTTP, selects a same-origin
HTTP+JSON 1.0 interface, and maps compatible advertised skills to Capabilities.
A direct Message becomes the result. For an asynchronous Task, the adapter
polls state and emits progress and artifacts; persistent NineA calls store those
events. The adapter requests remote cancellation only when the Task is still
active.

One A2A input Message is one NineA invocation. The current adapter does not
expose A2A streaming or multi-turn continuation. An asynchronous Task can take
time and emit progress without becoming a multi-turn conversation.

This keeps A2A semantics at the adapter edge. A consuming agent can use the
resulting Skill without embedding an A2A client or loading the full Agent Card
into every prompt.

See the official [A2A Protocol Specification](https://github.com/a2aproject/A2A/blob/main/docs/specification.md)
for the Agent Card, Message, Task, Artifact, streaming, and cancellation data
models.

## Adapter requirements

- Use absolute executable paths in operator configuration.
- Read credentials from the adapter's environment or an external secret store;
  never return them in discovery metadata.
- Reserve stdout for valid protocol messages and send diagnostics to stderr.
- Terminate every JSONL message with a newline and bound every line to 8 MiB.
- Bound descriptions, schemas, results, event counts, artifacts, and error
  messages.
- Set network and upstream request timeouts.
- Treat request input and upstream responses as untrusted data.
- Declare lifecycle behavior accurately. Version v1 must declare
  `multi_turn: false`; do not advertise streaming, cancellation, or idempotency
  that the upstream lacks.
- Exit non-zero when startup configuration is invalid.

NineA removes `NINEA_TOKEN` and `NINEA_BOOTSTRAP_TOKEN` before launching an
executable adapter. Do not reuse caller authentication as upstream
authentication. Use protocol-specific provider credentials instead.

Executable adapters run with the daemon user's OS privileges unless an
operator adds a stronger process or container boundary. See
[Security](SECURITY.md) before registering an adapter.
