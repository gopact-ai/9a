package provider

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestAdapterErrorValidationAndHumanText(t *testing.T) {
	err, validationErr := NewAdapterError("upstream_error", "safe message")
	if validationErr != nil || err.Error() != "upstream_error: safe message" {
		t.Fatalf("NewAdapterError()=%v, %v", err, validationErr)
	}
	if !err.Valid() {
		t.Fatal("constructed AdapterError is not valid")
	}
	var typed *AdapterError
	if !errors.As(err, &typed) || typed.Code() != "upstream_error" || typed.Message() != "safe message" {
		t.Fatalf("typed error=%#v", typed)
	}
	typeOfError := reflect.TypeOf(AdapterError{})
	if field, ok := typeOfError.FieldByName("Code"); ok && field.IsExported() {
		t.Fatal("AdapterError exposes writable Code field")
	}
	if field, ok := typeOfError.FieldByName("Message"); ok && field.IsExported() {
		t.Fatal("AdapterError exposes writable Message field")
	}
	for _, input := range []struct{ code, message string }{
		{"", "message"},
		{"code", ""},
		{"bad code", "message"},
		{strings.Repeat("x", 129), "message"},
		{"code", strings.Repeat("x", 1025)},
		{"code", "line\nbreak"},
	} {
		if _, err := NewAdapterError(input.code, input.message); err == nil {
			t.Fatalf("NewAdapterError(%q,%q) succeeded", input.code, input.message)
		}
	}
}

func TestAdapterErrorZeroAndNilValuesAreSafeAndInvalid(t *testing.T) {
	zero := &AdapterError{}
	if zero.Valid() || zero.Code() != "" || zero.Message() != "" || zero.Error() != "invalid adapter error" {
		t.Fatalf("zero AdapterError valid=%v code=%q message=%q error=%q", zero.Valid(), zero.Code(), zero.Message(), zero.Error())
	}
	var typedNil *AdapterError
	if typedNil.Valid() || typedNil.Code() != "" || typedNil.Message() != "" || typedNil.Error() != "invalid adapter error" {
		t.Fatalf("nil AdapterError valid=%v code=%q message=%q error=%q", typedNil.Valid(), typedNil.Code(), typedNil.Message(), typedNil.Error())
	}
}
