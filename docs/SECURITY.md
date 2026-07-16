# Security

## Report a vulnerability

Email `contact@gopact-ai.org` with subject `[SECURITY][9a]`.

Do not open a public Issue or Discussion for a suspected vulnerability. Do not
include live credentials, sensitive prompts, or upstream results in the first
report.

## Side-effect boundary

`9a search`, `9a status`, and read-only `9a doctor` operations do not contact an
upstream system. `9a run` is the explicit boundary that may cause an upstream
side effect.

Capabilities that may mutate upstream state require an approval token. HTTP
methods other than GET and capabilities with executable hooks always require
one; a side-effecting GET must declare `requiresApproval: true`; and all MCP
and A2A capabilities require one. NineA does not trust MCP
`readOnlyHint` as an authorization boundary. Without a valid token, NineA
creates no call and sends nothing upstream. The preflight token binds the
selected capability revision and exact input. It is single-use, expires after
10 minutes, and exists only in daemon memory, so a daemon restart invalidates
it. Review both bound values before using it; any change requires a new
preflight and explicit approval.

NineA runs with the current operating-system account's permissions. It is a
local capability runtime, not a sandbox or a multi-tenant isolation boundary.

## Local runtime and state

The CLI starts a private local runtime when needed. Its transport and state are
limited to the current operating-system account. The default state directory
is `$HOME/.local/state/ninea` with mode `0700`; sensitive runtime files use mode
`0600`.

Do not share the local state directory. NineA is not a security boundary
against another process running as the same operating-system account.

## Manifest and network validation

Integration manifests are untrusted input. NineA rejects unknown fields,
duplicate YAML keys, aliases, multiple documents, unsafe paths, malformed
templates, invalid JSON Schemas, and sources larger than 8 MiB.

Remote HTTP and A2A endpoints require HTTPS. Plain HTTP is accepted only for
loopback development endpoints. Service URLs cannot contain credentials,
queries, or fragments. HTTP redirects are limited and must remain on the
original origin. Requests use bounded timeouts, and responses are size-limited.

Input is validated against the capability's JSON Schema before the request.
Output is validated before a call is marked complete. External JSON Schema
references are rejected so validation cannot fetch remote documents.

## Credentials

Do not put secret values in a manifest. A manifest may declare an alias and a
reference such as `private-api.api-token`, then use
`{{ secrets.api-token }}` in request fields.

`9a secret set <integration>.<key>` reads the value from a hidden terminal
prompt or stdin and stores it in the operating system credential store (system
keyring). Values are scoped to the current workspace, so identical references
in two workspaces are independent. The local database stores only scoped
references and timestamps. `secret list`, status output, errors, and logs must
never expose values.

If an A2A Agent Card requires bearer authentication, declare one credential in
the A2A manifest and store it with `9a secret set`; do not pass it through the
process environment. See the [A2A manifest reference](reference/manifest.md#a2a).

## Local executables

An MCP integration and an executable hook run local code with the current OS
account's permissions. Review and sandbox executables when that trust level is
too broad.

MCP manifests accept one absolute executable without shell arguments. NineA
passes a minimal environment for executable lookup, temporary files, locale,
and TLS certificates; it does not forward cloud, source-control, or NineA
credential variables. MCP manifests cannot inject additional variables.
Executable hooks require `security.allowExecutableHooks: true`, an absolute
executable path, and an explicit environment-variable allowlist. NineA invokes
the argument array without a shell and bounds execution time and output, but an
allowed environment variable is still visible to that process.

## Persisted and workspace data

Accepted `run` requests create persistent call records. Inputs, results,
events, runtime metadata, and logs may contain sensitive business data. The
local state database is not encrypted at rest; protect the state directory and
apply external disk encryption when required. NineA bounds retained call count
and bytes, pruning the oldest terminal calls when admitting new work; active
calls are never pruned.

The canonical manifest is `.9a/integrations/<name>.yaml`. The shared
`.agents/skills/using-ninea` gateway is derived discovery state and is not a
security boundary. Processes running as the same OS account can inspect or
modify workspace files.
