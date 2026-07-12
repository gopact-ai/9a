# Declarative API Skills

Use declarative YAML for JSON HTTP APIs. Keep secrets in environment variables
inherited by `ninead`.

```yaml
apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: weather
  description: Read current weather.
variables:
  api-token:
    fromEnv: WEATHER_API_TOKEN
    sensitive: true
    required: false
services:
  weather:
    baseURL: https://api.open-meteo.com
    headers:
      Authorization: "Bearer {{ vars.api-token }}"
operations:
  current:
    description: Read weather for coordinates.
    service: weather
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
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body.current
```

Run:

```sh
9a validate weather.yaml
9a add weather.yaml
9a diff weather.yaml
printf '%s\n' '{"latitude":31.2,"longitude":121.5}' | \
  .agents/skills/weather/operations/current/invoke
```

One YAML document may define multiple services, operations, and sequential
workflows. Templates use `input`, `vars`, and prior `steps`. Request hooks can
set/remove headers or transform the request with embedded jq; response hooks
can shape one JSON result. Use executable hooks only for reviewed signing or
transformations that declarative actions cannot express, and explicitly set
`security.allowExecutableHooks: true`.

Use `9a remove weather` to remove the source and its managed view. Never edit
the projected `references/source.yaml`; update the original YAML and run
`9a add` again.
