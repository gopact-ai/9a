package capability

import (
	"errors"
	"regexp"
	"strings"
)

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

type Source struct {
	Protocol, Provider, UpstreamName string
}

type Contract struct {
	Mode       string
	JSONSchema map[string]any
	MediaTypes []string
}

type Lifecycle struct {
	Sync, Streaming, MultiTurn, Cancelable bool
	States                                 []string
}

type Security struct {
	RequiresApproval string
	UpstreamAuth     string
}

type Capability struct {
	ID, Kind, Name, Description string
	Source                      Source
	Input, Output               Contract
	Lifecycle                   Lifecycle
	Security                    Security
	Tags, Examples              []string
	RawMetadata                 []byte
	Revision                    int64
}

func Slug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.Trim(nonSlug.ReplaceAllString(s, "-"), "-")
	return s
}

func StableID(protocol, provider, upstream string) string {
	return Slug(protocol) + "/" + Slug(provider) + "/" + Slug(upstream)
}

func (c Capability) Validate() error {
	if c.ID == "" || c.Kind == "" || c.Name == "" || c.Description == "" {
		return errors.New("capability identity is incomplete")
	}
	if c.Source.Protocol == "" || c.Source.Provider == "" || c.Source.UpstreamName == "" {
		return errors.New("capability source is incomplete")
	}
	if Slug(c.Source.Protocol) == "" || Slug(c.Source.Provider) == "" || Slug(c.Source.UpstreamName) == "" {
		return errors.New("capability source cannot form a public reference")
	}
	if c.Input.Mode == "" || c.Output.Mode == "" {
		return errors.New("capability contracts are incomplete")
	}
	return nil
}
