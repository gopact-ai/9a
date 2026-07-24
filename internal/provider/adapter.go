// Package provider defines the Provider model and the Adapter interface that
// each integration protocol implements to discover, invoke, cancel,
// health-check, and close capabilities, along with the Sink, Event, and
// validated AdapterError types used to stream results back to the runtime.
package provider

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"unicode"
	"unicode/utf8"

	"github.com/gopact-ai/9a/internal/capability"
)

const (
	MaxAdapterErrorCodeBytes    = 128
	MaxAdapterErrorMessageBytes = 1024
)

var safeAdapterErrorCode = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

type adapterErrorValidity struct{ marker byte }

var constructedAdapterError = &adapterErrorValidity{marker: 1}

type AdapterError struct {
	code     string
	message  string
	validity *adapterErrorValidity
}

func (e *AdapterError) Valid() bool {
	return e != nil && e.validity == constructedAdapterError && validAdapterErrorFields(e.code, e.message)
}

func (e *AdapterError) Error() string {
	if !e.Valid() {
		return "invalid adapter error"
	}
	return e.code + ": " + e.message
}

func (e *AdapterError) Code() string {
	if !e.Valid() {
		return ""
	}
	return e.code
}

func (e *AdapterError) Message() string {
	if !e.Valid() {
		return ""
	}
	return e.message
}

func validAdapterErrorFields(code, message string) bool {
	if code == "" || len(code) > MaxAdapterErrorCodeBytes || !safeAdapterErrorCode.MatchString(code) {
		return false
	}
	if message == "" || len(message) > MaxAdapterErrorMessageBytes || !utf8.ValidString(message) {
		return false
	}
	for _, r := range message {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func NewAdapterError(code, message string) (*AdapterError, error) {
	if !validAdapterErrorFields(code, message) {
		if code == "" || len(code) > MaxAdapterErrorCodeBytes || !safeAdapterErrorCode.MatchString(code) {
			return nil, errors.New("invalid adapter error code")
		}
		return nil, errors.New("invalid adapter error message")
	}
	return &AdapterError{code: code, message: message, validity: constructedAdapterError}, nil
}

type Provider struct {
	ID, Protocol, Name, Endpoint string
	Config                       map[string]string
}

type Health struct {
	Healthy bool
	Message string
}

type Event struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

type Sink interface {
	Started() error
	Event(Event) error
	Artifact(name, mediaType string, data []byte) error
}

type Adapter interface {
	Discover(context.Context, Provider) ([]capability.Capability, error)
	Invoke(context.Context, Provider, capability.Capability, string, json.RawMessage, Sink) error
	Cancel(context.Context, Provider, string) error
	Health(context.Context, Provider) Health
	Close(context.Context, Provider) error
}
