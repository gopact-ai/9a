package declarative

import (
	"strings"
	"testing"
)

const validSource = `apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: weather
  description: Weather operations.
projection:
  targets: [.agents/skills]
variables:
  client:
    fromEnv: WEATHER_CLIENT
    default: ninea
services:
  forecast:
    baseURL: https://api.open-meteo.com
operations:
  current:
    service: forecast
    method: GET
    path: /v1/forecast
    request:
      query:
        latitude: "{{ input.latitude }}"
      headers:
        X-Client: "{{ vars.client }}"
    inputSchema:
      type: object
    hooks:
      beforeRequest:
        - setHeaders:
            X-Trace: "{{ vars.client }}"
      afterResponse:
        - transform:
            language: jq
            expression: '{temperature: .body.current.temperature_2m}'
workflows:
  report:
    inputSchema:
      type: object
    steps:
      - id: weather
        use: current
        input:
          latitude: "{{ input.latitude }}"
    output:
      language: jq
      expression: .steps.weather
`

func TestParseValidSource(t *testing.T) {
	c, err := Parse([]byte(validSource))
	if err != nil {
		t.Fatal(err)
	}
	if c.Metadata.Name != "weather" || len(c.Operations) != 1 || len(c.Workflows) != 1 {
		t.Fatalf("config=%#v", c)
	}
	if c.Digest == "" || c.SkillRoot() != ".agents/skills" {
		t.Fatalf("digest=%q root=%q", c.Digest, c.SkillRoot())
	}
}

func TestParseDigestTracksMeaningNotFormatting(t *testing.T) {
	first, err := Parse([]byte(validSource))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Parse([]byte("# operator note\n" + validSource))
	if err != nil {
		t.Fatal(err)
	}
	if first.Digest != second.Digest {
		t.Fatalf("digest changed for comment: %s != %s", first.Digest, second.Digest)
	}
}

func TestParseRejectsUnsafeOrAmbiguousYAML(t *testing.T) {
	tests := map[string]string{
		"duplicate": strings.Replace(validSource, "  name: weather", "  name: weather\n  name: other", 1),
		"unknown":   validSource + "unknown: true\n",
		"alias":     strings.Replace(validSource, "Weather operations.", "&description Weather operations.", 1) + "extra: *description\n",
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(source)); err == nil {
				t.Fatalf("unsafe YAML accepted")
			}
		})
	}
}

func TestParseRejectsInvalidReferencesAndSecurity(t *testing.T) {
	tests := map[string]string{
		"undeclared variable":   strings.Replace(validSource, "{{ vars.client }}", "{{ vars.missing }}", 1),
		"remote http":           strings.Replace(validSource, "https://api.open-meteo.com", "http://api.open-meteo.com", 1),
		"exec disabled":         strings.Replace(validSource, "beforeRequest:\n        - setHeaders:\n            X-Trace: \"{{ vars.client }}\"", "beforeRequest:\n        - exec:\n            command: [/bin/true]", 1),
		"malformed template":    strings.Replace(validSource, "{{ input.latitude }}", "{{ input.latitude", 1),
		"response header hook":  strings.Replace(validSource, "        - transform:\n            language: jq\n            expression: '{temperature: .body.current.temperature_2m}'", "        - setHeaders:\n            X-Late: invalid", 1),
		"unbounded timeout":     strings.Replace(validSource, "beforeRequest:\n        - setHeaders:\n            X-Trace: \"{{ vars.client }}\"", "beforeRequest:\n        - exec:\n            command: [/bin/true]\n            timeout: 2m", 1) + "security:\n  allowExecutableHooks: true\n",
		"invalid jq":            strings.Replace(validSource, "'{temperature: .body.current.temperature_2m}'", "'{'", 1),
		"invalid environment":   strings.Replace(validSource, "fromEnv: WEATHER_CLIENT", "fromEnv: 'BAD ENV'", 1),
		"duplicate header case": strings.Replace(validSource, "        X-Client: \"{{ vars.client }}\"", "        X-Client: one\n        x-client: two", 1),
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(source)); err == nil {
				t.Fatalf("invalid source accepted")
			}
		})
	}
}
