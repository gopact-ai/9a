# Security

## Report a vulnerability

Email `contact@gopact-ai.org` with subject `[SECURITY][9a]`.

Do not open a public Issue or Discussion for a suspected vulnerability. Do not
include live credentials, provider secrets, sensitive prompts, or tool results
in the initial report.

## Authentication and authorization

- `ninead` listens on a local Unix socket and changes the socket mode to
  `0600`. Every request still requires a bearer token; possession of the socket
  path is not authentication.
- Tokens identify callers. NineA stores SHA-256 token digests, not plaintext
  bearer tokens.
- The first daemon start imports `NINEA_BOOTSTRAP_TOKEN` as the administrator
  token only when the token store is empty. Later starts reject a non-empty
  bootstrap token.
- Capability access is default-deny. `read` controls search and projection;
  `invoke` independently controls execution. Adapter registration, provider
  registration, token creation, and ACL grants require `admin`.
- Persistent call records and events are visible only to the call owner or an
  administrator.

Use a distinct token for every agent and grant only the capabilities and
permissions it needs. Keep the state database, token files, socket parent
directory, projected Skill directories, and daemon logs private with operating
system permissions.

## Provider and adapter credentials

`NINEA_TOKEN` authenticates a client to NineA. It is not an upstream provider
credential. `ninead` removes `NINEA_TOKEN` and `NINEA_BOOTSTRAP_TOKEN` from its
environment after startup, and the MCP and executable adapter launchers also
strip both variables from child environments.

MCP servers and executable adapters inherit other daemon environment variables.
Store provider credentials in protocol-specific variables or an external
secret store:

- A2A provider `research-agent` uses
  `NINEA_A2A_TOKEN_RESEARCH_AGENT` when its Agent Card selects bearer auth.
- The generic HTTP provider `orders-api` uses
  `NINEA_HTTP_TOKEN_ORDERS_API` for manifest operations configured with
  `"auth":"bearer"`.

Declarative Skills name environment variables under `variables` and resolve
them only at invocation time. Templates such as `{{ vars.api-token }}` can set
service or request headers without embedding the value in YAML. The projected
`references/source.yaml` retains the environment variable name, not its value.

The generic HTTP adapter never sends an available token for an operation marked
`none`, and it does not forward caller headers. Do not put credentials in
provider descriptions, manifests committed to source control, Capability
metadata, schemas, projected Skills, or command arguments.

## Local process trust

MCP servers and registered executable adapters are trusted local code. They run
with the daemon user's OS privileges and can read environment variables and
files available to that user. NineA does not sandbox them.

Only administrators can register adapters. Registration requires a reviewed,
executable regular file at an absolute path; NineA resolves symlinks and stores
the canonical path. The file must remain trusted and unchanged after
registration. Use a dedicated OS account, container, or another process sandbox
when an integration needs a stronger boundary.

The built-in MCP adapter limits the daemon to 64 active stdio sessions across
all providers and reserves capacity before starting a server process. Discovery,
synchronous invocation, and persistent calls share this limit. Requests beyond
it are rejected without spawning another child process. The synchronous API
reports the rejection as `request_failed`; an already-created persistent call
records `failed` with code `resource_exhausted`.

## Network boundaries

The built-in declarative and A2A adapters and the generic HTTP adapter apply the
following network rules:

- non-loopback providers require HTTPS; cleartext HTTP is limited to loopback
  development endpoints;
- redirects are limited to three, must remain on the original origin, and may
  not downgrade HTTPS;
- endpoints cannot contain embedded credentials, and operation paths cannot
  change scheme or host;
- requests, responses, protocol messages, schemas, metadata, and events are
  bounded; upstream requests use timeouts;
- returned error messages are sanitized instead of echoing URLs, response
  bodies, or tokens.

Declarative YAML is strictly decoded and rejects unknown fields, duplicate
keys, aliases, multiple documents, malformed templates, remote cleartext HTTP,
unsafe paths, invalid references, and oversized source files before
installation. HTTP response bodies are bounded at 8 MiB.

Declarative executable hooks are disabled by default. Enabling them is an
explicit source-level trust decision. A hook command is an absolute argument
array executed without shell interpolation; it receives only allowlisted
environment variables and bounded JSON over stdin/stdout. NineA caps hook
timeouts and output, places the process in its own group, and kills that group
on cancellation or overflow. A global 32-process admission limit rejects excess
hooks before launch. Hook failures returned through the API do not include
stderr. Hook code is still trusted code with the daemon user's privileges and
is not a sandbox.

A2A Agent Card discovery does not send an operation bearer token. The adapter
accepts compatible public or HTTP Bearer security requirements and applies the
effective card or skill policy to each operation.

## Untrusted data and persistence

Provider descriptions, schemas, events, artifacts, and results are untrusted
data. NineA validates and bounds adapter messages and generated Skill metadata,
but consuming agents must still treat upstream text as data rather than trusted
instructions.

Call inputs, states, results, events, artifacts, adapter registrations,
providers, ACLs, and the Catalog are stored in SQLite. The current 0.x release
does not encrypt the database at rest. Logs and adapter stderr may also contain
provider diagnostics, so protect and review them.

Persistent call admission is capped at 8 active calls, 1,000 retained calls,
and 256 MiB of retained call data per identity. Database-wide limits are 64
active calls, 10,000 retained calls, and 2 GiB. Retained data includes inputs,
results, and event envelopes. Terminal calls release active capacity but remain
charged to retained count and bytes. There is no automatic retention cleanup or
call deletion API, so monitor these limits and archive or replace the state
database offline before retained capacity is exhausted.

Reading projected files has no provider side effect. NineA never mounts over or
deletes a user-owned Skill directory. FUSE projections reject write, truncate,
rename, delete, chmod, and xattr mutation with read-only filesystem semantics.

The portable directory backend uses read-only modes, atomic replacement, and a
versioned ownership manifest containing path, mode, size, and SHA-256 for every
file. It detects and repairs drift but is not a security boundary against the
same OS account or root, which can change its own permissions. Require the FUSE
backend when kernel-enforced immutability matters.

Running a projected invocation command crosses back into the authenticated
NineA runtime and requires `invoke` permission. `detach` removes only registry-
verified managed views and preserves providers, sources, ACLs, and call data.
