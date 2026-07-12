# Declarative Skill examples

- [`open-meteo.yaml`](open-meteo.yaml) is runnable without an API key and
  combines geocoding with weather in one workflow.
- [`authenticated-api.yaml`](authenticated-api.yaml) shows environment-backed
  bearer authentication, request headers, GET and POST mappings, and response
  shaping.
- [`api-bundle.yaml`](api-bundle.yaml) groups three API domains behind one
  Skill and composes two of them into a workflow.
- [`executable-hook.yaml`](executable-hook.yaml) is the opt-in escape hatch for
  logic that cannot be expressed with headers and jq. Replace the example
  command with the absolute path to [`hooks/sign-request.py`](hooks/sign-request.py).

Validate any example before adding it:

```sh
9a validate examples/declarative/open-meteo.yaml
9a add examples/declarative/open-meteo.yaml
```

The complete schema and hook contract are documented in
[Declarative Skills](../../docs/declarative-skills.md).
