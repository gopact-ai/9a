# Generic HTTP API adapter

This standard-library Go executable connects JSON HTTP APIs to NineA without
changing or rebuilding NineA. Edit a manifest to describe operations, then
register the executable through NineA's language-neutral adapter registry.

The example is intended as a secure, production-oriented starting point rather
than a full OpenAPI client.

## Build

From the NineA repository root:

```sh
mkdir -p "$HOME/.local/bin" "$HOME/.config/ninea"
go build -o "$HOME/.local/bin/ninea-http-adapter" \
  ./examples/http-adapter
cp examples/http-adapter/manifest.example.json \
  "$HOME/.config/ninea/http-manifest.json"
```

Edit the copied manifest before registration.

## Manifest

The adapter loads `NINEA_HTTP_ADAPTER_MANIFEST` once at process startup. The
file is strict JSON, limited to 8 MiB, and has this top-level shape:

```json
{
  "version": "1",
  "health_path": "/healthz",
  "health_auth": "none",
  "operations": [
    {
      "upstream_name": "get-order",
      "name": "Get order",
      "description": "Returns one order by ID.",
      "method": "GET",
      "path": "/v1/orders",
      "input_schema": {
        "type": "object",
        "properties": {
          "id": {
            "type": "string"
          }
        }
      },
      "output_schema": {
        "type": "object"
      },
      "auth": "bearer",
      "requires_approval": "never"
    }
  ]
}
```

Each operation declares:

- a unique canonical `upstream_name`;
- bounded `name`, `description`, optional `tags`, and optional `examples`;
- one of `GET`, `POST`, `PUT`, `PATCH`, or `DELETE`;
- a root-relative path with no host, query, fragment, or parent traversal;
- object-valued `input_schema` and `output_schema` metadata;
- `auth` set to `none` or `bearer`;
- `requires_approval` set to `always` or `never`.

NineA preserves schemas and approval metadata for agents, but the current
runtime does not evaluate JSON Schema or implement an approval workflow. The
operator remains responsible for policy around operations marked `always`.

See [manifest.example.json](manifest.example.json) for complete operations.

## Credentials and daemon environment

Provider names must be canonical slugs. The bearer token name is derived from
the provider name: `orders-api` uses `NINEA_HTTP_TOKEN_ORDERS_API`.

```sh
export NINEA_HTTP_ADAPTER_MANIFEST="$HOME/.config/ninea/http-manifest.json"
export NINEA_HTTP_TOKEN_ORDERS_API='replace-with-provider-token'
```

These variables must be present in the `9a daemon` environment. Exporting them in
a different shell after the daemon is running does not update an existing
adapter process. Restart the daemon when changing the manifest or its provider
credentials.

An operation with `"auth":"none"` never sends an available bearer token.
`health_auth` independently controls authentication for `health_path` and
defaults to `none` when a health path is present. The adapter never forwards
NineA caller headers, `NINEA_TOKEN`, or `NINEA_BOOTSTRAP_TOKEN` upstream.

## Register and use

With an administrator token in `NINEA_TOKEN`:

```sh
9a adapters add httpapi "$HOME/.local/bin/ninea-http-adapter"
9a providers add httpapi orders-api https://api.example.com
```

The adapter registration and provider are persisted in NineA's state database.
The executable must remain at its registered absolute path after restart.

Grant an agent access to discovered capabilities:

```sh
AGENT_TOKEN="$(9a tokens create order-agent)"
9a acl grant order-agent httpapi/orders-api/get-order read,invoke
```

Then use that agent token to search, project, or invoke:

```sh
export NINEA_TOKEN="$AGENT_TOKEN"
9a search "get order"
9a project add httpapi/orders-api/get-order .agents/skills
printf '%s\n' '{"id":"ord_123"}' | \
  9a invoke httpapi/orders-api/get-order
```

The generic adapter reports synchronous, non-streaming, single-turn, and
non-cancelable capabilities. `calls start` can still persist and track an
invocation, but `calls cancel` is not available for these operations.

## HTTP behavior

- GET and DELETE require an input object containing only string, number,
  boolean, or null values. Keys are encoded as deterministic query parameters.
- POST, PUT, and PATCH send the input as an `application/json` body.
- Responses must use `application/json` or an `application/*+json` media type
  and contain valid JSON.
- Provider endpoints may use HTTP only on loopback. Remote providers require
  HTTPS.
- Redirects are limited to three, must remain on the original origin, and may
  not downgrade HTTPS.
- Requests use a 30-second timeout. URLs are limited to 16 KiB; request,
  response, manifest, and JSONL protocol messages are bounded at 8 MiB.
- At most 32 invokes run concurrently in one adapter process. Additional
  invokes return `resource_exhausted` without contacting the provider.
- Upstream failures return stable, sanitized errors without response bodies,
  URLs, or tokens.

## Current limits

This example is JSON-only. It does not upload files, forward arbitrary headers,
stream responses, continue multi-turn conversations, evaluate JSON Schema,
enforce approval metadata, or cancel upstream requests. MCP and A2A integrations
use their built-in adapters instead.
