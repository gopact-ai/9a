package a2a

import (
	"bytes"
	"encoding/json"
	"errors"
)

const (
	protocolVersion = "1.0"
	maxBodyBytes    = 8 << 20
	maxSkills       = 10_000
	maxStringBytes  = 4 << 10
	maxListItems    = 1_000
)

type agentInterface struct {
	URL             string `json:"url"`
	ProtocolBinding string `json:"protocolBinding"`
	Tenant          string `json:"tenant,omitempty"`
	ProtocolVersion string `json:"protocolVersion"`
}

type agentSkill struct {
	ID                   string                     `json:"id"`
	Name                 string                     `json:"name"`
	Description          string                     `json:"description"`
	Tags                 []string                   `json:"tags"`
	Examples             []string                   `json:"examples,omitempty"`
	InputModes           []string                   `json:"inputModes,omitempty"`
	OutputModes          []string                   `json:"outputModes,omitempty"`
	SecurityRequirements []agentSecurityRequirement `json:"securityRequirements,omitempty"`
}

type agentCapabilities struct {
	Extensions []agentExtension `json:"extensions,omitempty"`
}

type agentExtension struct {
	URI         string          `json:"uri"`
	Description string          `json:"description,omitempty"`
	Required    bool            `json:"required,omitempty"`
	Params      json.RawMessage `json:"params,omitempty"`
}

type agentCard struct {
	Name                 string                         `json:"name"`
	Description          string                         `json:"description"`
	SupportedInterfaces  []agentInterface               `json:"supportedInterfaces"`
	Version              string                         `json:"version"`
	Capabilities         json.RawMessage                `json:"capabilities"`
	SecuritySchemes      map[string]agentSecurityScheme `json:"securitySchemes,omitempty"`
	SecurityRequirements []agentSecurityRequirement     `json:"securityRequirements,omitempty"`
	DefaultInputModes    []string                       `json:"defaultInputModes"`
	DefaultOutputModes   []string                       `json:"defaultOutputModes"`
	Skills               []agentSkill                   `json:"skills"`
}

type resolvedProvider struct {
	baseURL     string
	tenant      string
	authBySkill map[string]bool
}

type agentSecurityScheme struct {
	HTTPAuth json.RawMessage `json:"httpAuthSecurityScheme,omitempty"`
	APIKey   json.RawMessage `json:"apiKeySecurityScheme,omitempty"`
	OAuth2   json.RawMessage `json:"oauth2SecurityScheme,omitempty"`
	OpenID   json.RawMessage `json:"openIdConnectSecurityScheme,omitempty"`
	MTLS     json.RawMessage `json:"mtlsSecurityScheme,omitempty"`
}

type agentSecurityRequirement struct {
	Schemes map[string]agentStringList `json:"schemes"`
}

type agentStringList struct {
	List []string `json:"list"`
}

func (s *agentStringList) UnmarshalJSON(data []byte) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil || object == nil || len(object) > 1 {
		return errors.New("invalid A2A StringList")
	}
	raw, exists := object["list"]
	if !exists {
		if len(object) != 0 {
			return errors.New("invalid A2A StringList")
		}
		s.List = nil
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &s.List) != nil || len(s.List) > maxListItems {
		return errors.New("invalid A2A StringList")
	}
	for _, value := range s.List {
		if value == "" || len(value) > maxStringBytes {
			return errors.New("invalid A2A StringList")
		}
	}
	return nil
}

type httpAuthSecurityScheme struct {
	Scheme string `json:"scheme"`
}
