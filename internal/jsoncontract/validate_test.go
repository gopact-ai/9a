package jsoncontract

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestValidateJSONContract(t *testing.T) {
	t.Parallel()
	schema := map[string]any{
		"type":     "object",
		"required": []any{"city"},
		"properties": map[string]any{
			"city": map[string]any{"type": "string", "minLength": 1},
		},
	}
	if err := Validate(schema, json.RawMessage(`{"city":"Shanghai"}`)); err != nil {
		t.Fatal(err)
	}
	if err := Validate(schema, json.RawMessage(`{"city":1}`)); err == nil || !strings.Contains(err.Error(), "city") {
		t.Fatalf("invalid value error=%v", err)
	}
}

func TestCompileRejectsInvalidOrExternalSchema(t *testing.T) {
	t.Parallel()
	tests := []map[string]any{
		{"type": "not-a-json-type"},
		{"$ref": "https://example.com/schema.json"},
	}
	for _, schema := range tests {
		if err := Compile(schema); err == nil {
			t.Fatalf("Compile(%v) accepted invalid schema", schema)
		}
	}
}
