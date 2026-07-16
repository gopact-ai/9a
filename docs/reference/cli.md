# CLI reference

NineA has seven public command groups: `connect`, `search`, `run`, `status`,
`disconnect`, `doctor`, and `secret`. Add `--json` to a command for
machine-readable output.

## connect

With no arguments, print the three first-connect routes without starting the
runtime. A fresh agent can load the embedded authoring contract directly:

```sh
9a connect
9a connect --guide <http|mcp|a2a> --json
```

The JSON guide is local and contains `type`, `manifestVersion`, an editable
`template`, the full embedded `guide`, and `nextAction`. It does not create a
socket, database, integration, or workspace Skill.

Connect a version 1 manifest:

```sh
9a connect <manifest.yaml>
```

NineA validates and creates or updates the integration in the current
workspace. It copies the validated source to
`.9a/integrations/<name>.yaml`, which becomes the canonical source.
On success, human output suggests `9a search <integration> --json`, which lists
every visible capability in that integration.

Two shortcuts create the protocol-specific manifest for you:

```sh
9a connect mcp --name <name> -- /absolute/path/to/server
9a connect a2a --name <name> <https-url>
```

The MCP shortcut accepts one absolute executable without arguments. The A2A
shortcut requires HTTPS, except that loopback HTTP is accepted for local
development. Both save a canonical v1 source under `.9a/integrations`. An A2A
agent that requires bearer authentication must use a manifest; see the
[A2A manifest reference](manifest.md#a2a).

## search

```sh
9a search <query...>
```

Find capabilities visible to the current workspace. Results include a short
runnable `<integration>/<capability>` reference.

A single canonical integration name deterministically lists every readable
capability in that integration. If no visible integration has that name, NineA
uses the text as a normal full-text query:

```sh
9a search weather --json
```

Use an exact reference with `--json` to inspect its input and output contracts:

```sh
9a search weather/current-weather --json
```

## run

```sh
9a run <integration>/<capability> [--input <json|@file|->] \
  [--approve <token>]
```

Input defaults to `{}`. `--input -` reads stdin; a piped JSON value is also
accepted. Do not combine two input sources.

NineA validates input before contacting the upstream system and persists every
accepted call. HTTP methods other than GET, capabilities that use executable
hooks, and GET capabilities declaring `requiresApproval: true` require
approval. Every MCP and A2A capability requires approval. If it is missing,
nothing is sent upstream and the JSON error includes
`data.approvalToken`. Review the same input, obtain explicit user approval, then
retry the identical command with `--approve <token>`. The token binds the
capability revision and exact input. It is single-use, expires after 10 minutes,
and is invalidated by a daemon restart; changing either bound value also
requires a new preflight.

## status

```sh
9a status [integration]
```

Show whether the current workspace or one integration is `ready`,
`needs-secret`, or `broken`. An empty workspace is reported as not ready.
Missing credentials and broken integrations include a next command such as
`9a secret set <integration>.<key>` or `9a doctor`.

Use `--workspace <directory>` to inspect a workspace other than the one
resolved from the current directory.

## disconnect

```sh
9a disconnect <integration>
```

Remove the integration from the active runtime. Its canonical source remains
at `.9a/integrations/<name>.yaml`, and the workspace keeps the shared
`using-ninea` gateway Skill.

## doctor

```sh
9a doctor [--workspace <directory>] [--fix]
```

Diagnose runtime, workspace, canonical-source, and integration-state problems.
The default is read-only and prints a next action for each problem. `--fix`
reconnects sources when needed; that can start a local MCP executable or contact
an A2A endpoint for discovery.

## secret

```sh
9a secret set <integration>.<key>
9a secret list [integration]
9a secret unset <integration>.<key>
```

`set` reads a value from a hidden terminal prompt or stdin; values are not
accepted as command arguments. `list` prints references and whether each value
is present, never the values themselves. `unset` removes the value from the
operating system credential store. All three operate on the current workspace;
the same reference may hold a different value in another workspace.

## Machine-readable output

Add `--json` to a public command to receive JSON instead of human-readable
output. See [Errors and side effects](errors.md) for the stable error envelope,
approval retry contract, and `sideEffect` semantics.
