# Architecture

NineA is a protocol-neutral capability layer between upstream systems and AI
agents. It converts capabilities from MCP tools, A2A agents, and APIs into a
searchable Catalog, then projects selected capabilities as filesystem-native
Skills.

```text
                    discover
MCP / A2A / API ─────────────→ Adapter
                                  │
                                  ▼
                         normalized Capability
                                  │
                  registry + index + ACL + revision
                                  ▼
                      SQLite state + Catalog
                                  │
                               project
                                  ▼
                         filesystem Skill
                                  │
                         explicit command
                                  ▼
Agent ─────────────────────→ NineA runtime ─────────→ Adapter ─→ upstream
```

NineA includes built-in MCP and A2A adapters. The executable adapter registry
extends the same capability model to additional protocols without defining a
new agent-facing interface.

## Implementation model

### Adapter

An adapter owns the protocol-specific edge. It discovers upstream operations,
translates them into NineA Capabilities, invokes them, reports health, and
cancels work when the upstream protocol supports cancellation.

The built-in MCP and A2A adapters are Go implementations of NineA's internal
adapter interface. Separately installed protocol integrations use the
`9a.adapter/v1` executable contract, so they can be written in any language,
registered without recompiling `ninead`, and isolated as separate processes.
The generic HTTP adapter is an example of this external executable model.

The executable adapter contract is defined in [Building adapters](adapters.md).
The current alpha supports runtime registration of executable adapters, a
built-in MCP stdio adapter, single-turn A2A HTTP+JSON 1.0 agents, and a generic
HTTP API adapter example. Streaming and multi-turn continuation are not yet
supported.

The built-in MCP adapter globally admits at most 64 active stdio sessions.
Discovery, synchronous invocation, and persistent calls share that capacity
across providers. Capacity is reserved before process start; an additional
session is rejected without spawning a child. The synchronous API reports that
adapter rejection as `request_failed`. A persistent call is already admitted
and returns its ID before adapter invocation; if MCP capacity is then exhausted,
the call becomes `failed` with code `resource_exhausted`. Completion,
cancellation, provider close, and failed process start release the reservation.

### Capability

A Capability is the protocol-neutral description of something an agent can do.
It includes:

- a stable identity, name, kind, and description;
- its adapter protocol, provider, and upstream name;
- input and output contracts;
- synchronous, streaming, multi-turn, and cancellation properties;
- approval and upstream authentication metadata;
- tags, examples, revision, and bounded upstream metadata.

NineA owns the normalized identity and source fields. An adapter cannot claim
another provider's namespace.

### Catalog

The Catalog is a persistent, revisioned inventory of Capabilities. It lets
NineA hold many capabilities without placing every definition in an agent's
prompt or Skill directory.

Provider records and executable adapter registrations are also persistent.
During restore, NineA loads and validates executable registrations before
restoring providers that depend on them. A missing or invalid registered
executable fails restore instead of silently publishing an unusable provider.

Search runs locally, applies the caller's `read` ACL before returning results,
and never contacts or invokes an upstream provider. This gives agents
progressive capability disclosure: search first, then load the complete Skill
only when it becomes relevant.

### Skill projection

Projection materializes one visible Capability as an ordinary Agent Skill:

```text
ninea-mcp-weather-get-weather/
  SKILL.md
  schema.json
  references/upstream.json
  scripts/invoke
```

The files contain instructions, a machine-readable contract, bounded
provenance, and a small invocation entry point. NineA refuses to overwrite
paths it does not own.

Projection is deliberately selective. The Catalog may contain thousands of
capabilities while an agent's Skill directory contains only the few needed for
its current work.

### Runtime, calls, and authorization

Discovery, reading, and execution are separate operations:

- `read` permits search and projection;
- `invoke` permits execution;
- `admin` permits adapter, provider, token, and ACL management.

Reading a projected Skill has no upstream side effect. The invocation script
sends structured input to `ninead`, which authenticates the caller, checks the
Capability ACL, locates the originating provider, and routes the call through
its adapter.

This separation preserves filesystem semantics: inspecting instructions is
passive, while running a command is an explicit action.

NineA offers two execution paths over the same adapter invocation:

- synchronous `invoke` waits for a terminal result in the client request;
- `calls start` persists the input and returns a call ID, while a background
  invocation records state, result, events, and artifacts in SQLite.

Call records and event pages are visible only to their owner or an
administrator. Event sequence numbers, individual payloads, total event count,
aggregate bytes, and page size are bounded. Cancellation is attempted only for
an active call whose Capability declares `cancelable`; terminal ownership is
resolved before a canceled state is persisted.

Terminal call records, results, and events survive daemon restart. A clean
shutdown completes active calls as `failed` with code `app_closed`. If a crash
or another interruption leaves an active record in SQLite, restore completes
that record as `failed` with code `daemon_restarted`; the work is not resumed.

Admission is calculated from persisted SQLite state, so concurrency limits and
retained-storage limits survive restart:

| Scope | Active calls | Retained calls | Retained bytes |
| --- | ---: | ---: | ---: |
| One identity | 8 | 1,000 | 256 MiB |
| Whole database | 64 | 10,000 | 2 GiB |

Retained bytes count call inputs, terminal results, and persisted event
envelopes. A terminal transition releases active capacity, but the call record
and all of its bytes remain retained. This release has no call deletion,
retention expiry, or garbage collection; an exhausted retained quota requires
offline archival or replacement of the state database before new calls can be
admitted.

## Why command line and filesystem

AI agents already treat files and commands as first-class working interfaces.
They can navigate repositories, read instructions, inspect schemas, compose
shell pipelines, and run narrowly scoped programs without learning a new UI or
embedding a new SDK.

NineA assigns a distinct responsibility to each interface:

| Interface | Responsibility |
| --- | --- |
| Filesystem | Discovery, instructions, schemas, provenance, and local composition |
| Command line | Explicit invocation, structured input/output, and visible side effects |
| NineA socket | Authentication, authorization, routing, and request transport |
| Adapter process | Upstream protocol, credentials, and result translation |

This boundary is useful for both agents and people. The same Skill can be
inspected in an editor, audited with ordinary file tools, invoked from a shell,
or loaded by an Agent Skills implementation. No vendor-specific tool registry
is required on the consumer side.

Files do not replace the runtime. Representing a remote action only as writable
files would blur inspection and execution. NineA instead uses files for passive
capability disclosure and commands for active operations.

## The Plan 9 connection

Plan 9 from Bell Labs organized a distributed system around three closely
related ideas:

1. resources are named and accessed like files in a hierarchy;
2. a standard protocol, 9P, accesses local and remote file services;
3. separate hierarchies are assembled into a process-specific namespace.

The important lesson is not the slogan "everything is a file." It is that
heterogeneous resources become easier to understand and combine when they are
presented through a small, consistent interface and assembled into the user's
own namespace.

NineA applies that lesson to agent capabilities:

```text
Plan 9: distributed resources  → file-like services → private namespace
NineA: distributed capabilities → Skills            → agent namespace
```

NineA does not implement 9P and does not claim that remote actions are files.
It borrows the namespace principle: adapters hide heterogeneous protocols, and
operators can project a local, inspectable Skill view for each agent.

In the current alpha, `read` authorization gates search and projection, but the
result is an ordinary caller-selected directory. NineA does not enforce access
to those files after projection. Operators that need isolated agent views must
use separate directories and operating-system permissions.

Primary references:

- [Plan 9 from Bell Labs](https://9p.io/sys/doc/9.html)
- [The Use of Name Spaces in Plan 9](https://9p.io/sys/doc/names.html)

## End-to-end data flow

### Discovery

1. An administrator registers a provider under an adapter protocol.
2. NineA asks the adapter to discover upstream operations.
3. The adapter returns protocol-neutral capability descriptions.
4. The current alpha validates required identity and contract fields. The
   executable adapter contract additionally requires schema shape, lifecycle,
   and message-bound validation before external adapters are accepted.
5. The Catalog atomically replaces that provider's previous revision.

### Projection

1. An agent searches the Catalog using its own token.
2. NineA filters results using the agent's `read` permission.
3. The agent selects one Capability.
4. NineA generates an owned Skill directory without contacting the provider.

### Invocation

1. The agent reads the Skill and prepares JSON input.
2. The agent explicitly runs `scripts/invoke`.
3. `ninead` authenticates the token and checks `invoke` permission.
4. NineA routes the request through the Capability's adapter and provider.
5. The adapter translates the upstream result into a structured NineA result.

### Persistent call

1. The agent sends JSON input with `calls start` and receives a call ID.
2. NineA persists the submitted record before launching the adapter invocation.
3. Adapter events and artifacts are appended with monotonic sequence numbers.
4. `calls get` reads state and result; `calls events` reads bounded pages.
5. If supported, `calls cancel` asks the active adapter invocation to confirm
   cancellation before NineA records a canceled terminal state.

The Catalog and projected Skill remain useful even if the upstream provider is
temporarily unavailable. Only invocation depends on the live upstream system.
