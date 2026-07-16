package jsoncontract

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	jsonschema "github.com/santhosh-tekuri/jsonschema/v6"
)

const MaxSchemaBytes = 1 << 20

var (
	ErrInvalidSchema = errors.New("invalid json schema")
	ErrInvalidValue  = errors.New("json value does not match schema")
)

func Compile(definition map[string]any) error {
	_, err := compile(definition)
	return err
}

func Validate(definition map[string]any, value json.RawMessage) error {
	schema, err := compile(definition)
	if err != nil {
		return err
	}
	instance, err := jsonschema.UnmarshalJSON(bytes.NewReader(value))
	if err != nil {
		return fmt.Errorf("%w: decode json value: %v", ErrInvalidValue, err)
	}
	if err := schema.Validate(instance); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidValue, err)
	}
	return nil
}

func compile(definition map[string]any) (*jsonschema.Schema, error) {
	if definition == nil {
		definition = map[string]any{}
	}
	if err := rejectExternalReferences(definition); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidSchema, err)
	}
	raw, err := json.Marshal(definition)
	if err != nil {
		return nil, fmt.Errorf("%w: encode: %v", ErrInvalidSchema, err)
	}
	if len(raw) > MaxSchemaBytes {
		return nil, fmt.Errorf("%w: exceeds %d bytes", ErrInvalidSchema, MaxSchemaBytes)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("%w: decode: %v", ErrInvalidSchema, err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource("schema.json", document); err != nil {
		return nil, fmt.Errorf("%w: add resource: %v", ErrInvalidSchema, err)
	}
	compiled, err := compiler.Compile("schema.json")
	if err != nil {
		return nil, fmt.Errorf("%w: compile: %v", ErrInvalidSchema, err)
	}
	return compiled, nil
}

func rejectExternalReferences(value any) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, item := range typed {
			if key == "$ref" {
				reference, ok := item.(string)
				if !ok || !strings.HasPrefix(reference, "#") {
					return errors.New("json schema external references are not allowed")
				}
			}
			if err := rejectExternalReferences(item); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := rejectExternalReferences(item); err != nil {
				return err
			}
		}
	}
	return nil
}
