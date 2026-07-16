# Integration examples

- [`open-meteo.yaml`](open-meteo.yaml) is runnable without a credential and
  combines geocoding with weather in one workflow.
- [`authenticated-http.yaml`](authenticated-http.yaml) declares a credential
  alias and injects its value into an HTTP header at run time.
- [`executable-hook.yaml`](executable-hook.yaml) is the opt-in escape hatch for
  logic that cannot be expressed with headers and jq. Replace its command with
  the absolute path to [`hooks/sign-request.py`](hooks/sign-request.py); the
  allowlisted environment variable is supplied directly to that executable.

Connect and run the public example from any workspace:

```sh
9a connect /path/to/open-meteo.yaml
9a search weather/city-weather --json
9a run weather/city-weather --input '{"city":"Shanghai"}'
```

For the authenticated example, connect before storing the declared reference:

```sh
9a connect /path/to/authenticated-http.yaml
9a secret set private-api.api-token
9a status private-api
9a search private-api/current-user --json
9a run private-api/current-user
```

`secret set` reads from a hidden prompt or stdin. The value is stored in the
operating system credential store and never in the manifest.

`connect` copies each validated source to
`.9a/integrations/<integration>.yaml`. That copy is the canonical source;
`disconnect` keeps it.

MCP and A2A integrations have shorter connect forms:

```sh
9a connect mcp --name local-tools -- /absolute/path/to/server
9a connect a2a --name research-agent https://agent.example.com
```
