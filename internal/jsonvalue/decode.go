// Package jsonvalue decodes a single JSON value while preserving numbers as
// json.Number rather than converting them to float64, and rejects trailing
// data after the value.
package jsonvalue

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

// Decode preserves JSON numbers instead of silently converting them to
// float64 when they are stored in interface values.
func Decode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
