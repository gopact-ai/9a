# Architecture

NineA is a local capability runtime. It gives HTTP APIs, MCP servers, and A2A
agents one workspace-scoped search and execution interface.

```text
.9a/integrations/<name>.yaml ── connect ──→ validated capability index
                                                    ▲
.agents/skills/using-ninea ──→ human or agent ── search / inspect ──┘
                                      └── run ──→ upstream system
```

## Public model

- An **integration** is one external system and one canonical source.
- A **capability** is one runnable action, addressed as
  `<integration>/<capability>`.
- A **workspace** is the project that owns the integration sources and exposes
  those capabilities to its agent.

Protocol names, internal identifiers, and process details stay behind this
interface.

## State ownership

| State | Role | Editable |
| --- | --- | --- |
| `.9a/integrations/<name>.yaml` | Canonical integration source | Yes |
| Operating system credential store (system keyring) | Secret values | Through `9a secret` |
| Local database | Connected-source index, cached capability contracts, secret metadata, and call records | No |
| `.agents/skills/using-ninea` | Shared agent gateway | No |

`connect` strictly validates a source before replacing the active integration.
A failed update restores the last working source and runtime registration.
`disconnect` removes only the active registration; it retains the canonical
source and shared gateway Skill.

For integration configuration, the database is only an index of canonical
workspace sources and their cached capability contracts. It never replaces a
manifest and never contains secret values.

Startup restore is passive. NineA uses the database to locate sources that were
already connected, validates those local files, and restores HTTP definitions
locally. It does not scan for unconnected manifests, start an MCP executable,
or contact an A2A endpoint. MCP and A2A keep their last validated capability
contracts when the source still matches the last `connect`. A missing, invalid,
or mismatched source is reported as broken instead of triggering upstream
discovery.

## Integration types

- HTTP manifests compile declared services, capabilities, workflows, hooks,
  and JSON Schemas into runtime entries.
- MCP integrations start one absolute executable and discover its tools.
- A2A integrations connect to an HTTPS endpoint and discover the remote
  agent's capabilities.

All three expose the same short references. Callers use `search` and `run`
without learning protocol-specific execution details.

NineA installs one `using-ninea` gateway Skill per workspace. It contains the
stable workflow for `status`, `search`, and `run`. All integrations share it;
there are no capability-specific Skill directories.

## Execution path

1. `run` resolves the short capability reference in the workspace index.
2. NineA verifies that it belongs to the current workspace and checks whether
   explicit approval is required.
3. NineA validates the JSON input and creates a persistent call record.
4. The selected integration runtime invokes the upstream system.
5. NineA validates the result schema before marking the call complete.

Invalid input and missing approval are rejected before contacting the upstream
system. Accepted calls have a stable call ID so failures can be correlated with
persisted state. Credential aliases are resolved from the operating system
credential store at invocation time.

## Local process boundary

The CLI communicates with an automatically started local runtime. It owns
capability lookup, call persistence, credential resolution, and upstream
execution. Searching, status, and read-only diagnosis remain passive. During
normal capability use, `run` crosses the upstream side-effect boundary.
