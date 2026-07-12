package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

const (
	maxManifestBytes       = 8 << 20
	maxManifestOperations  = 10_000
	maxManifestStringBytes = 4 << 10
	maxManifestListItems   = 1_000
	maxManifestSchemaBytes = 1 << 20
	maxManifestMetadata    = 1 << 20
	maxDescriptionRunes    = 512
)

var canonicalName = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

type manifest struct {
	Version    string      `json:"version"`
	HealthPath string      `json:"health_path,omitempty"`
	HealthAuth string      `json:"health_auth,omitempty"`
	Operations []operation `json:"operations"`

	byUpstream map[string]operation
}

type operation struct {
	UpstreamName     string         `json:"upstream_name"`
	Name             string         `json:"name"`
	Description      string         `json:"description"`
	Method           string         `json:"method"`
	Path             string         `json:"path"`
	InputSchema      map[string]any `json:"input_schema"`
	OutputSchema     map[string]any `json:"output_schema"`
	Tags             []string       `json:"tags,omitempty"`
	Examples         []string       `json:"examples,omitempty"`
	Auth             string         `json:"auth"`
	RequiresApproval string         `json:"requires_approval"`
}

func loadManifest(filename string) (*manifest, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(data) > maxManifestBytes {
		return nil, errors.New("manifest exceeds 8 MiB limit")
	}
	if !utf8.Valid(data) {
		return nil, errors.New("manifest is not valid UTF-8")
	}
	if err := rejectDuplicateObjectKeys(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	var parsed manifest
	if err := decoder.Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode manifest: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if err := parsed.validate(); err != nil {
		return nil, err
	}
	return &parsed, nil
}

func rejectDuplicateObjectKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil {
		return errors.New("manifest is not valid JSON")
	}
	if err := walkJSONValue(decoder, first); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("manifest must contain exactly one JSON value")
	}
	return nil
}

func walkJSONValue(decoder *json.Decoder, token json.Token) error {
	delimiter, composite := token.(json.Delim)
	if !composite {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return errors.New("manifest is not valid JSON")
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("manifest object key is invalid")
			}
			if _, duplicate := seen[key]; duplicate {
				return errors.New("manifest contains a duplicate object key")
			}
			seen[key] = struct{}{}
			value, err := decoder.Token()
			if err != nil {
				return errors.New("manifest is not valid JSON")
			}
			if err := walkJSONValue(decoder, value); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("manifest object is not closed")
		}
	case '[':
		for decoder.More() {
			value, err := decoder.Token()
			if err != nil {
				return errors.New("manifest is not valid JSON")
			}
			if err := walkJSONValue(decoder, value); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("manifest array is not closed")
		}
	default:
		return errors.New("manifest has an invalid JSON delimiter")
	}
	return nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("manifest must contain exactly one JSON value")
	}
	return nil
}

func (m *manifest) validate() error {
	if m.Version != "1" {
		return errors.New("manifest version must be 1")
	}
	if len(m.Operations) == 0 || len(m.Operations) > maxManifestOperations {
		return errors.New("manifest operations count is invalid")
	}
	if m.HealthPath == "" {
		if m.HealthAuth != "" {
			return errors.New("health_auth requires health_path")
		}
	} else {
		if err := validateRelativeRootedPath(m.HealthPath); err != nil {
			return errors.New("invalid health_path")
		}
		if m.HealthAuth == "" {
			m.HealthAuth = "none"
		}
		if !validAuth(m.HealthAuth) {
			return errors.New("health_auth must be none or bearer")
		}
	}
	lookup := make(map[string]operation, len(m.Operations))
	for i := range m.Operations {
		item := &m.Operations[i]
		if err := item.validate(); err != nil {
			return fmt.Errorf("invalid operation %d: %w", i+1, err)
		}
		if _, duplicate := lookup[item.UpstreamName]; duplicate {
			return errors.New("operation upstream_name values must be unique")
		}
		lookup[item.UpstreamName] = cloneOperation(*item)
	}
	m.byUpstream = lookup
	return nil
}

func (o *operation) validate() error {
	o.Name = strings.TrimSpace(o.Name)
	o.Description = strings.TrimSpace(o.Description)
	if !canonicalName.MatchString(o.UpstreamName) || len(o.UpstreamName) > maxManifestStringBytes {
		return errors.New("upstream_name must be a canonical slug")
	}
	if !validManifestString(o.Name) || !validManifestString(o.Description) || utf8.RuneCountInString(o.Description) > maxDescriptionRunes {
		return errors.New("name and description are required bounded strings")
	}
	switch o.Method {
	case "GET", "POST", "PUT", "PATCH", "DELETE":
	default:
		return errors.New("method is not supported")
	}
	if err := validateRelativeRootedPath(o.Path); err != nil {
		return errors.New("path is invalid")
	}
	if o.InputSchema == nil || o.OutputSchema == nil {
		return errors.New("input_schema and output_schema must be JSON objects")
	}
	for _, schema := range []map[string]any{o.InputSchema, o.OutputSchema} {
		encoded, err := json.Marshal(schema)
		if err != nil || len(encoded) > maxManifestSchemaBytes {
			return errors.New("schema exceeds limit")
		}
	}
	if !validAuth(o.Auth) {
		return errors.New("auth must be none or bearer")
	}
	if o.RequiresApproval != "always" && o.RequiresApproval != "never" {
		return errors.New("requires_approval must be always or never")
	}
	var err error
	if o.Tags, err = normalizeStringList(o.Tags); err != nil {
		return errors.New("tags and examples must contain bounded strings")
	}
	if o.Examples, err = normalizeStringList(o.Examples); err != nil {
		return errors.New("tags and examples must contain bounded strings")
	}
	encoded, err := json.Marshal(o)
	if err != nil || len(encoded) > maxManifestMetadata {
		return errors.New("operation metadata exceeds limit")
	}
	return nil
}

func validAuth(value string) bool { return value == "none" || value == "bearer" }

func validManifestString(value string) bool {
	return value != "" && len(value) <= maxManifestStringBytes && utf8.ValidString(value)
}

func normalizeStringList(values []string) ([]string, error) {
	if len(values) > maxManifestListItems {
		return nil, errors.New("list exceeds limit")
	}
	normalized := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if !validManifestString(value) {
			return nil, errors.New("invalid list value")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		normalized = append(normalized, value)
	}
	return normalized, nil
}

func validateRelativeRootedPath(value string) error {
	if value == "" || len(value) > maxManifestStringBytes || !utf8.ValidString(value) || !strings.HasPrefix(value, "/") || strings.HasPrefix(value, "//") {
		return errors.New("path must be relative-rooted")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "" || parsed.Host != "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return errors.New("path contains forbidden URL components")
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return errors.New("path traversal is forbidden")
		}
	}
	return nil
}

func cloneOperation(value operation) operation {
	value.Tags = append([]string(nil), value.Tags...)
	value.Examples = append([]string(nil), value.Examples...)
	value.InputSchema = cloneJSONObject(value.InputSchema)
	value.OutputSchema = cloneJSONObject(value.OutputSchema)
	return value
}

func cloneJSONObject(value map[string]any) map[string]any {
	encoded, _ := json.Marshal(value)
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var cloned map[string]any
	_ = decoder.Decode(&cloned)
	return cloned
}
