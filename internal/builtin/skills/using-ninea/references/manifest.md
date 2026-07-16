# HTTP integration manifest

Use this strict format:

```yaml
version: 1
name: weather
description: Read current weather.
type: http

services:
  forecast:
    baseURL: https://api.open-meteo.com

capabilities:
  current-weather:
    description: Read weather for coordinates.
    service: forecast
    method: GET
    path: /v1/forecast
    request:
      query:
        latitude: "{{ input.latitude }}"
        longitude: "{{ input.longitude }}"
        current: temperature_2m
    inputSchema:
      type: object
      required: [latitude, longitude]
      additionalProperties: false
      properties:
        latitude: {type: number}
        longitude: {type: number}
    outputSchema:
      type: object
      required: [temperature_2m]
      properties:
        temperature_2m: {type: number}
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body.current
```

`version`, `name`, `type`, `services`, and `capabilities` are required. Every
capability and workflow must explicitly declare both `inputSchema` and
`outputSchema`; use `{}` only when accepting any JSON is intentional. Names use
lowercase kebab-case. Remote services require HTTPS; loopback HTTP is allowed
for development.

Templates may read `input`. Sequential workflows may also read prior `steps`.
Embedded jq can shape a request or response. Executable hooks require explicit
opt-in and reviewed absolute executable paths. jq structural transforms
preserve arbitrary-size integers and precision-sensitive decimals; arithmetic
on a decimal that cannot be represented exactly fails instead of rounding.

Unknown fields, duplicate keys, aliases, multiple YAML documents, malformed
templates, and unsafe URLs are errors. Do not add secret values to a manifest.

For authentication, declare an alias whose reference belongs to the same
integration, then interpolate only the alias:

```yaml
credentials:
  api-token:
    secret: weather.api-token
services:
  forecast:
    baseURL: https://api.example.com
    headers:
      Authorization: "Bearer {{ secrets.api-token }}"
```

After connecting, ask the user to run `9a secret set weather.api-token`. The
value is scoped to the current workspace. Never ask for it in chat or include
it in a command argument.

After authoring:

```sh
9a connect integration.yaml
9a search weather/current-weather --json
9a run weather/current-weather --input '{"latitude":31.2,"longitude":121.5}'
```

HTTP methods other than GET and any operation with an executable hook require
an approval preflight. Set `requiresApproval: true` beside `method` for a GET
that charges, sends, starts work, or otherwise changes state. Run once without
`--approve`, ask the user to approve the exact input, then retry unchanged with
the returned `data.approvalToken`.
