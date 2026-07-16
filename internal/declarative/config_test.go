package declarative

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validSource = `version: 1
name: weather
description: Weather operations.
type: http
services:
  forecast:
    baseURL: https://api.open-meteo.com
capabilities:
  current:
    service: forecast
    method: GET
    path: /v1/forecast
    request:
      query:
        latitude: "{{ input.latitude }}"
      headers:
        X-Client: ninea
    inputSchema:
      type: object
    outputSchema:
      type: object
    hooks:
      beforeRequest:
        - setHeaders:
            X-Trace: ninea
      afterResponse:
        - transform:
            language: jq
            expression: '{temperature: .body.current.temperature_2m}'
workflows:
  report:
    inputSchema:
      type: object
    outputSchema:
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
	if c.Name != "weather" || c.Type != "http" || len(c.Capabilities) != 1 || len(c.Workflows) != 1 {
		t.Fatalf("config=%#v", c)
	}
	if c.Digest == "" {
		t.Fatal("digest is empty")
	}
}

func TestParsePreservesJSONNumbers(t *testing.T) {
	source := `version: 1
name: exact-numbers
type: http
services:
  api:
    baseURL: https://example.com
capabilities:
  echo:
    service: api
    method: POST
    path: /echo
    request:
      query:
        id: 123456789012345678901234567890
      body:
        amount: 0.123456789012345678901
    inputSchema:
      type: integer
      maximum: 123456789012345678901234567890
    outputSchema: {}
`
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	operation := config.Capabilities["echo"]
	assertJSONNumber(t, operation.Request.Query["id"], "123456789012345678901234567890")
	assertJSONNumber(t, operation.Request.Body.(map[string]any)["amount"], "0.123456789012345678901")
	assertJSONNumber(t, operation.InputSchema["maximum"], "123456789012345678901234567890")
}

func assertJSONNumber(t *testing.T, value any, want string) {
	t.Helper()
	number, ok := value.(json.Number)
	if !ok || number.String() != want {
		t.Fatalf("number=%T(%v), want json.Number(%s)", value, value, want)
	}
}

func TestParseAcceptsDeclaredSecretTemplates(t *testing.T) {
	source := strings.Replace(validSource, "type: http\n", "type: http\ncredentials:\n  api-key:\n    secret: weather.api-key\n", 1)
	source = strings.Replace(source, "    baseURL: https://api.open-meteo.com", "    baseURL: https://api.open-meteo.com\n    headers:\n      Authorization: 'Bearer {{ secrets.api-key }}'", 1)
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if got := config.Credentials["api-key"].Secret; got != "weather.api-key" {
		t.Fatalf("secret reference=%q", got)
	}
}

func TestParseAcceptsProtocolManifests(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	canonicalExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		source     string
		wantType   string
		wantTarget string
	}{
		{
			name:       "mcp",
			source:     fmt.Sprintf("version: 1\nname: local-tools\ntype: mcp\nexecutable: %q\n", executable),
			wantType:   "mcp",
			wantTarget: canonicalExecutable,
		},
		{
			name:       "a2a https",
			source:     "version: 1\nname: research-agent\ntype: a2a\nurl: https://agent.example.com/a2a\ncredentials:\n  bearer:\n    secret: research-agent.bearer-token\n",
			wantType:   "a2a",
			wantTarget: "https://agent.example.com/a2a",
		},
		{
			name:       "a2a loopback http",
			source:     "version: 1\nname: local-agent\ntype: a2a\nurl: http://127.0.0.1:8080/a2a\n",
			wantType:   "a2a",
			wantTarget: "http://127.0.0.1:8080/a2a",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config, err := Parse([]byte(test.source))
			if err != nil {
				t.Fatal(err)
			}
			if config.Type != test.wantType {
				t.Fatalf("type=%q", config.Type)
			}
			if test.wantType == "mcp" && config.Executable != test.wantTarget {
				t.Fatalf("executable=%q", config.Executable)
			}
			if test.wantType == "a2a" && config.URL != test.wantTarget {
				t.Fatalf("url=%q", config.URL)
			}
		})
	}
}

func TestParseRejectsInvalidProtocolManifests(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	notExecutable := t.TempDir() + "/not-executable"
	if err := os.WriteFile(notExecutable, []byte("not executable"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := map[string]string{
		"unknown type":             "version: 1\nname: tools\ntype: grpc\n",
		"mcp missing executable":   "version: 1\nname: tools\ntype: mcp\n",
		"mcp relative executable":  "version: 1\nname: tools\ntype: mcp\nexecutable: ./server\n",
		"mcp missing file":         "version: 1\nname: tools\ntype: mcp\nexecutable: /definitely/not/a/9a-executable\n",
		"mcp directory":            fmt.Sprintf("version: 1\nname: tools\ntype: mcp\nexecutable: %q\n", t.TempDir()),
		"mcp non-executable":       fmt.Sprintf("version: 1\nname: tools\ntype: mcp\nexecutable: %q\n", notExecutable),
		"mcp args":                 fmt.Sprintf("version: 1\nname: tools\ntype: mcp\nexecutable: %q\nargs: [serve]\n", executable),
		"mcp a2a field":            fmt.Sprintf("version: 1\nname: tools\ntype: mcp\nexecutable: %q\nurl: ''\n", executable),
		"mcp http field":           fmt.Sprintf("version: 1\nname: tools\ntype: mcp\nexecutable: %q\nservices: {}\n", executable),
		"mcp credential":           fmt.Sprintf("version: 1\nname: tools\ntype: mcp\nexecutable: %q\ncredentials:\n  token:\n    secret: tools.token\n", executable),
		"a2a missing url":          "version: 1\nname: agent\ntype: a2a\n",
		"a2a remote http":          "version: 1\nname: agent\ntype: a2a\nurl: http://agent.example.com\n",
		"a2a userinfo":             "version: 1\nname: agent\ntype: a2a\nurl: https://user:pass@agent.example.com\n",
		"a2a query":                "version: 1\nname: agent\ntype: a2a\nurl: https://agent.example.com?a=1\n",
		"a2a fragment":             "version: 1\nname: agent\ntype: a2a\nurl: https://agent.example.com#card\n",
		"a2a mcp field":            "version: 1\nname: agent\ntype: a2a\nurl: https://agent.example.com\nexecutable: ''\n",
		"a2a http field":           "version: 1\nname: agent\ntype: a2a\nurl: https://agent.example.com\ncapabilities: {}\n",
		"a2a multiple credentials": "version: 1\nname: agent\ntype: a2a\nurl: https://agent.example.com\ncredentials:\n  first:\n    secret: agent.first\n  second:\n    secret: agent.second\n",
		"http mcp field":           validSource + "executable: ''\n",
		"http a2a field":           validSource + "url: ''\n",
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(source)); err == nil {
				t.Fatal("invalid protocol manifest accepted")
			}
		})
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
		"duplicate":       strings.Replace(validSource, "name: weather", "name: weather\nname: other", 1),
		"unknown":         validSource + "unknown: true\n",
		"alias":           strings.Replace(validSource, "Weather operations.", "&description Weather operations.", 1) + "extra: *description\n",
		"non-JSON number": strings.Replace(validSource, `latitude: "{{ input.latitude }}"`, "latitude: .nan", 1),
		"old envelope": `apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: weather
`,
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
		"remote http":               strings.Replace(validSource, "https://api.open-meteo.com", "http://api.open-meteo.com", 1),
		"exec disabled":             strings.Replace(validSource, "beforeRequest:\n        - setHeaders:\n            X-Trace: ninea", "beforeRequest:\n        - exec:\n            command: [/bin/true]", 1),
		"malformed template":        strings.Replace(validSource, "{{ input.latitude }}", "{{ input.latitude", 1),
		"response header hook":      strings.Replace(validSource, "        - transform:\n            language: jq\n            expression: '{temperature: .body.current.temperature_2m}'", "        - setHeaders:\n            X-Late: invalid", 1),
		"unbounded timeout":         strings.Replace(validSource, "beforeRequest:\n        - setHeaders:\n            X-Trace: ninea", "beforeRequest:\n        - exec:\n            command: [/bin/true]\n            timeout: 2m", 1) + "security:\n  allowExecutableHooks: true\n",
		"invalid jq":                strings.Replace(validSource, "'{temperature: .body.current.temperature_2m}'", "'{'", 1),
		"invalid input schema":      strings.Replace(validSource, "      type: object", "      type: not-a-json-type", 1),
		"external schema reference": strings.Replace(validSource, "      type: object", "      $ref: https://example.com/schema.json", 1),
		"duplicate header case":     strings.Replace(validSource, "        X-Client: ninea", "        X-Client: one\n        x-client: two", 1),
		"capability name collision": strings.Replace(validSource, "workflows:\n  report:", "workflows:\n  current:", 1),
		"unknown secret alias":      strings.Replace(validSource, "X-Client: ninea", "X-Client: '{{ secrets.api-key }}'", 1),
		"invalid secret reference":  strings.Replace(validSource, "type: http\n", "type: http\ncredentials:\n  api-key:\n    secret: weather\n", 1),
		"foreign secret reference":  strings.Replace(validSource, "type: http\n", "type: http\ncredentials:\n  api-key:\n    secret: other.api-key\n", 1),
		"inline secret value":       strings.Replace(validSource, "type: http\n", "type: http\ncredentials:\n  api-key:\n    secret: weather.api-key\n    value: never-allow-this\n", 1),
	}
	for name, source := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(source)); err == nil {
				t.Fatalf("invalid source accepted")
			}
		})
	}
}

func TestParseRequiresExplicitCapabilityAndWorkflowSchemas(t *testing.T) {
	tests := []struct {
		name   string
		remove string
		want   string
	}{
		{name: "capability input", remove: "    inputSchema:\n      type: object\n", want: `capability "current" inputSchema is required`},
		{name: "capability output", remove: "    outputSchema:\n      type: object\n", want: `capability "current" outputSchema is required`},
		{name: "workflow input", remove: "    inputSchema:\n      type: object\n", want: `workflow "report" inputSchema is required`},
		{name: "workflow output", remove: "    outputSchema:\n      type: object\n", want: `workflow "report" outputSchema is required`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := validSource
			if strings.HasPrefix(test.name, "workflow") {
				index := strings.Index(source, "workflows:")
				if index < 0 {
					t.Fatal("workflow fixture is missing")
				}
				source = source[:index] + strings.Replace(source[index:], test.remove, "", 1)
			} else {
				source = strings.Replace(source, test.remove, "", 1)
			}
			_, err := Parse([]byte(source))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Parse() error=%v, want %q", err, test.want)
			}
		})
	}
}

func TestParseAcceptsExplicitUnconstrainedSchemas(t *testing.T) {
	source := strings.ReplaceAll(validSource, "    inputSchema:\n      type: object\n", "    inputSchema: {}\n")
	source = strings.ReplaceAll(source, "    outputSchema:\n      type: object\n", "    outputSchema: {}\n")
	if _, err := Parse([]byte(source)); err != nil {
		t.Fatalf("Parse() rejected explicit unconstrained schemas: %v", err)
	}
}

func TestIntegrationExamplesParse(t *testing.T) {
	paths, err := filepath.Glob("../../examples/integrations/*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("integration examples are missing")
	}
	for _, path := range paths {
		t.Run(filepath.Base(path), func(t *testing.T) {
			source, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Parse(source); err != nil {
				t.Fatalf("Parse() example error=%v", err)
			}
		})
	}
}
