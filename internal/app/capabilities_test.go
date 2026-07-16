package app

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gopact-ai/9a/internal/capability"
)

func TestSameCapabilitiesUsesJSONNumberSemantics(t *testing.T) {
	left := []capability.Capability{{
		ID: "api/ws-demo/weather/current",
		Input: capability.Contract{JSONSchema: map[string]any{
			"type": "string", "minLength": 1,
		}},
	}}
	right := []capability.Capability{{
		ID: "api/ws-demo/weather/current",
		Input: capability.Contract{JSONSchema: map[string]any{
			"type": "string", "minLength": float64(1),
		}},
	}}
	if !sameCapabilities(left, right) {
		t.Fatal("JSON-equivalent schemas were treated as different")
	}
}

func TestPublicCapabilityPreservesExplicitEmptySchemas(t *testing.T) {
	item := capability.Capability{
		Source: capability.Source{Provider: "weather", UpstreamName: "current"},
		Input:  capability.Contract{Mode: "json", JSONSchema: map[string]any{}},
		Output: capability.Contract{Mode: "json", JSONSchema: map[string]any{}},
	}
	result := publicSearchResult(item, true)
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), `"schema":{}`); got != 2 {
		t.Fatalf("exact search contract = %s; want two explicit empty schemas", data)
	}
}

func TestPublicCapabilityNormalizesMissingSchemasToObjects(t *testing.T) {
	item := capability.Capability{
		Source: capability.Source{Provider: "research-agent", UpstreamName: "summarize"},
		Input:  capability.Contract{Mode: "json"},
		Output: capability.Contract{Mode: "a2a.response"},
	}
	data, err := json.Marshal(publicSearchResult(item, true))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(data), `"schema":{}`); got != 2 || strings.Contains(string(data), `"schema":null`) {
		t.Fatalf("exact search contract = %s", data)
	}
}
