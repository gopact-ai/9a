# Connect an HTTP integration

Create a version 1 HTTP manifest using the
[manifest reference](../reference/manifest.md), or ask an agent to translate
API documentation into one.

On a fresh workspace, the agent can load the same embedded contract without a
running daemon or projected Skill:

```sh
9a connect --guide http --json
```

```sh
9a connect ./weather.yaml
```

On success, NineA saves the source under `.9a/integrations`, prepares the
workspace, and prints a deterministic integration search:

```text
Connected weather (1 capability)

Next:
  9a search weather --json

Source:
  .9a/integrations/weather.yaml
```

That search lists every capability in the integration. Inspect a selected
contract with `9a search weather/<capability> --json` before constructing input.

Update the canonical source and run `connect` again. The operation is
idempotent. If validation or compilation fails, the last working source and
runtime remain active.

## Add a credential

The manifest declares an alias and where that value will be used:

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

Connect first, then store the missing value:

```sh
9a connect ./private-api.yaml
9a secret set private-api.api-token
9a status private-api
```

`secret set` reads from a hidden terminal prompt. It also accepts stdin for
automation; the value is never a command-line argument:

```sh
printf '%s' "$API_TOKEN" | 9a secret set private-api.api-token
```

Secret values are stored in the operating system credential store (system
keyring) for the current workspace. The same reference in another workspace is
a different value. NineA resolves it when a capability runs, so setting or
rotating it does not require reconnecting the integration.

## Run and disconnect

GET capabilities can run directly:

```sh
9a run weather/current-weather --input @request.json
```

If a GET charges, sends, starts work, or otherwise changes state, set
`requiresApproval: true` beside its `method`; the HTTP verb alone cannot express
that behavior safely.

For a capability that may change the upstream system, first run without an
approval token. NineA sends nothing upstream and returns `data.approvalToken`:

```sh
9a run orders/create-order --input @order.json --json
```

After the user explicitly approves that exact input, retry it unchanged:

```sh
9a run orders/create-order --input @order.json \
  --approve "$APPROVAL_TOKEN"
```

Changing the input or capability revision invalidates the token.

Disconnect without deleting the source:

```sh
9a disconnect weather
```

If `status` reports a broken integration, run `9a doctor` for a read-only
diagnosis. Review its findings before `9a doctor --fix`: reconnecting a stale
source may start a local MCP executable or contact an A2A endpoint.
