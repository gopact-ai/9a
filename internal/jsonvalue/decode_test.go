package jsonvalue

import (
	"encoding/json"
	"testing"
)

func TestDecodePreservesLargeIntegers(t *testing.T) {
	const input = `{"id":9007199254740993}`
	var value map[string]any
	if err := Decode([]byte(input), &value); err != nil {
		t.Fatal(err)
	}
	if number, ok := value["id"].(json.Number); !ok || number.String() != "9007199254740993" {
		t.Fatalf("decoded number=%T(%v)", value["id"], value["id"])
	}
	encoded, err := json.Marshal(value)
	if err != nil || string(encoded) != input {
		t.Fatalf("round trip=%s error=%v", encoded, err)
	}
}

func TestDecodeRejectsMultipleValues(t *testing.T) {
	var value any
	if err := Decode([]byte(`{} {}`), &value); err == nil {
		t.Fatal("multiple JSON values accepted")
	}
}
