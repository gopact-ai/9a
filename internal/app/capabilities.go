package app

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
)

type PublicContract struct {
	Mode       string         `json:"mode"`
	JSONSchema map[string]any `json:"schema"`
	MediaTypes []string       `json:"mediaTypes,omitempty"`
}

type CapabilitySearchResult struct {
	Ref              string          `json:"ref"`
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	RequiresApproval bool            `json:"requiresApproval"`
	Input            *PublicContract `json:"input,omitempty"`
	Output           *PublicContract `json:"output,omitempty"`
}

func publicCapabilityRef(item capability.Capability) string {
	return capability.Slug(item.Source.Provider) + "/" + capability.Slug(item.Source.UpstreamName)
}

func capabilityProviderID(item capability.Capability) string {
	index := strings.LastIndex(item.ID, "/")
	if index <= 0 {
		return ""
	}
	return item.ID[:index]
}

func scopeIntegrationCapabilities(p provider.Provider, items []capability.Capability) []capability.Capability {
	result := append([]capability.Capability(nil), items...)
	for i := range result {
		result[i].ID = p.ID + "/" + capability.Slug(result[i].Source.UpstreamName)
	}
	return result
}

func exactPublicRef(value string) bool {
	if strings.TrimSpace(value) != value || len(strings.Fields(value)) != 1 {
		return false
	}
	parts := strings.Split(value, "/")
	return len(parts) == 2 && parts[0] != "" && parts[1] != "" && capability.Slug(parts[0]) == parts[0] && capability.Slug(parts[1]) == parts[1]
}

func publicSearchResult(item capability.Capability, includeContracts bool) CapabilitySearchResult {
	result := CapabilitySearchResult{
		Ref:              publicCapabilityRef(item),
		Name:             item.Name,
		Description:      item.Description,
		RequiresApproval: item.Security.RequiresApproval == "always",
	}
	if includeContracts {
		result.Input = &PublicContract{Mode: item.Input.Mode, JSONSchema: publicJSONSchema(item.Input.JSONSchema), MediaTypes: item.Input.MediaTypes}
		result.Output = &PublicContract{Mode: item.Output.Mode, JSONSchema: publicJSONSchema(item.Output.JSONSchema), MediaTypes: item.Output.MediaTypes}
	}
	return result
}

func publicJSONSchema(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{}
	}
	return schema
}

func sameCapabilities(left, right []capability.Capability) bool {
	if len(left) != len(right) {
		return false
	}
	normalize := func(values []capability.Capability) []capability.Capability {
		out := append([]capability.Capability(nil), values...)
		for i := range out {
			out[i].Revision = 0
		}
		sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
		return out
	}
	leftJSON, leftErr := json.Marshal(normalize(left))
	rightJSON, rightErr := json.Marshal(normalize(right))
	return leftErr == nil && rightErr == nil && bytes.Equal(leftJSON, rightJSON)
}
