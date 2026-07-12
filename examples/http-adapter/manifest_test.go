package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validManifestMap() map[string]any {
	return map[string]any{
		"version":     "1",
		"health_path": "/healthz",
		"health_auth": "bearer",
		"operations": []any{
			map[string]any{
				"upstream_name":     "get-order",
				"name":              "Get order",
				"description":       "Returns one order.",
				"method":            "GET",
				"path":              "/v1/orders",
				"input_schema":      map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}},
				"output_schema":     map[string]any{"type": "object"},
				"tags":              []any{"orders", "read"},
				"examples":          []any{"Look up order 123"},
				"auth":              "bearer",
				"requires_approval": "never",
			},
		},
	}
}

func writeManifest(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadManifestPreservesSchemasAndBuildsImmutableLookup(t *testing.T) {
	manifest, err := loadManifest(writeManifest(t, validManifestMap()))
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "1" || manifest.HealthPath != "/healthz" || manifest.HealthAuth != "bearer" {
		t.Fatalf("manifest=%#v", manifest)
	}
	operation, ok := manifest.byUpstream["get-order"]
	if !ok || operation.Method != "GET" || operation.Auth != "bearer" || operation.RequiresApproval != "never" {
		t.Fatalf("operation=%#v ok=%v", operation, ok)
	}
	if got := operation.InputSchema["properties"].(map[string]any)["id"].(map[string]any)["type"]; got != "string" {
		t.Fatalf("input schema=%#v", operation.InputSchema)
	}
	manifest.Operations[0].Name = "mutated"
	if operation.Name != "Get order" {
		t.Fatal("lookup aliases mutable operations slice")
	}
}

func TestLoadManifestNormalizesMetadataAndPreservesJSONNumbers(t *testing.T) {
	value := validManifestMap()
	item := value["operations"].([]any)[0].(map[string]any)
	item["name"] = "  Get order  "
	item["description"] = "  Returns one order.  "
	item["tags"] = []any{" orders ", "read", "orders"}
	item["examples"] = []any{" Example one ", "Example one"}
	item["input_schema"].(map[string]any)["minimum"] = json.Number("9007199254740993")
	loaded, err := loadManifest(writeManifest(t, value))
	if err != nil {
		t.Fatal(err)
	}
	operation := loaded.byUpstream["get-order"]
	if operation.Name != "Get order" || operation.Description != "Returns one order." || strings.Join(operation.Tags, ",") != "orders,read" || strings.Join(operation.Examples, ",") != "Example one" {
		t.Fatalf("operation=%#v", operation)
	}
	minimum, ok := operation.InputSchema["minimum"].(json.Number)
	if !ok || minimum.String() != "9007199254740993" {
		t.Fatalf("minimum=%T(%v)", operation.InputSchema["minimum"], operation.InputSchema["minimum"])
	}
}

func TestLoadManifestRejectsInvalidConfiguration(t *testing.T) {
	long := strings.Repeat("x", maxManifestStringBytes+1)
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown top level field", mutate: func(m map[string]any) { m["token"] = "secret" }},
		{name: "wrong version", mutate: func(m map[string]any) { m["version"] = "2" }},
		{name: "missing operations", mutate: func(m map[string]any) { delete(m, "operations") }},
		{name: "too many operations", mutate: func(m map[string]any) { m["operations"] = make([]any, maxManifestOperations+1) }},
		{name: "duplicate upstream", mutate: func(m map[string]any) {
			operations := m["operations"].([]any)
			m["operations"] = append(operations, operations[0])
		}},
		{name: "noncanonical upstream", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["upstream_name"] = "Get_Order" }},
		{name: "empty display name", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["name"] = "" }},
		{name: "long description", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["description"] = long }},
		{name: "too many description runes", mutate: func(m map[string]any) {
			m["operations"].([]any)[0].(map[string]any)["description"] = strings.Repeat("界", 513)
		}},
		{name: "oversized input schema", mutate: func(m map[string]any) {
			m["operations"].([]any)[0].(map[string]any)["input_schema"] = map[string]any{"constant": strings.Repeat("x", (1<<20)+1)}
		}},
		{name: "unknown operation field", mutate: func(m map[string]any) {
			m["operations"].([]any)[0].(map[string]any)["headers"] = map[string]any{"Authorization": "secret"}
		}},
		{name: "unsupported method", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["method"] = "OPTIONS" }},
		{name: "lowercase method", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["method"] = "get" }},
		{name: "absolute URL path", mutate: func(m map[string]any) {
			m["operations"].([]any)[0].(map[string]any)["path"] = "https://evil.example/v1"
		}},
		{name: "network path", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["path"] = "//evil.example/v1" }},
		{name: "relative path", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["path"] = "v1/orders" }},
		{name: "parent path", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["path"] = "/v1/../admin" }},
		{name: "encoded parent path", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["path"] = "/v1/%2e%2e/admin" }},
		{name: "path query", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["path"] = "/v1/orders?admin=true" }},
		{name: "bad auth", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["auth"] = "basic" }},
		{name: "bad approval", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["requires_approval"] = "sometimes" }},
		{name: "schema not object", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["input_schema"] = []any{} }},
		{name: "empty tag", mutate: func(m map[string]any) { m["operations"].([]any)[0].(map[string]any)["tags"] = []any{""} }},
		{name: "health auth without path", mutate: func(m map[string]any) { delete(m, "health_path") }},
		{name: "bad health auth", mutate: func(m map[string]any) { m["health_auth"] = "basic" }},
		{name: "bad health path", mutate: func(m map[string]any) { m["health_path"] = "/../secret" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			manifest := validManifestMap()
			test.mutate(manifest)
			if _, err := loadManifest(writeManifest(t, manifest)); err == nil {
				t.Fatal("loadManifest accepted invalid configuration")
			}
		})
	}
}

func TestLoadManifestRejectsOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", maxManifestBytes+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadManifest(path); err == nil {
		t.Fatal("loadManifest accepted oversized file")
	}
}

func TestLoadManifestRejectsDuplicateObjectKeys(t *testing.T) {
	encoded, err := json.Marshal(validManifestMap())
	if err != nil {
		t.Fatal(err)
	}
	for _, data := range [][]byte{
		bytes.Replace(encoded, []byte(`"version":"1"`), []byte(`"version":"1","version":"1"`), 1),
		bytes.Replace(encoded, []byte(`"auth":"bearer"`), []byte(`"auth":"none","auth":"bearer"`), 1),
		bytes.Replace(encoded, []byte(`"type":"object"`), []byte(`"type":"array","type":"object"`), 1),
	} {
		path := filepath.Join(t.TempDir(), "manifest.json")
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadManifest(path); err == nil {
			t.Fatal("loadManifest accepted duplicate JSON object key")
		}
	}
}

func TestExampleManifestLoads(t *testing.T) {
	manifest, err := loadManifest("manifest.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Operations) != 2 || manifest.byUpstream["get-order"].Auth != "bearer" || manifest.byUpstream["create-order"].RequiresApproval != "always" {
		t.Fatalf("manifest=%#v", manifest)
	}
}
