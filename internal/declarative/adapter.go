package declarative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/jsonvalue"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/secret"
)

type Adapter struct {
	mu       sync.RWMutex
	sources  map[string]*Config
	resolver secret.Resolver
}

func NewAdapter() *Adapter {
	return NewAdapterWithResolver(nil)
}

func NewAdapterWithResolver(resolver secret.Resolver) *Adapter {
	return &Adapter{sources: make(map[string]*Config), resolver: resolver}
}

func (a *Adapter) Register(p provider.Provider, config *Config) error {
	if config == nil {
		return errors.New("integration config is missing")
	}
	if p.Protocol != "api" || p.Name != config.Name {
		return errors.New("integration identity does not match its manifest")
	}
	a.mu.Lock()
	a.sources[p.ID] = config
	a.mu.Unlock()
	return nil
}

func (a *Adapter) Snapshot(providerID string) *Config {
	a.mu.RLock()
	config := a.sources[providerID]
	a.mu.RUnlock()
	return config
}

func (a *Adapter) Unregister(providerID string) {
	a.mu.Lock()
	delete(a.sources, providerID)
	a.mu.Unlock()
}

func (a *Adapter) source(p provider.Provider) (*Config, error) {
	a.mu.RLock()
	config := a.sources[p.ID]
	a.mu.RUnlock()
	if config == nil {
		return nil, fmt.Errorf("integration %q is not loaded", p.Name)
	}
	return config, nil
}

func (a *Adapter) Discover(_ context.Context, p provider.Provider) ([]capability.Capability, error) {
	config, err := a.source(p)
	if err != nil {
		return nil, err
	}
	result := make([]capability.Capability, 0, len(config.Capabilities)+len(config.Workflows))
	for name, operation := range config.Capabilities {
		result = append(result, makeCapability(config, name, "operation", operation.Description, operation.InputSchema, operation.OutputSchema, operationRequiresApproval(operation), operationUsesSecret(config, operation)))
	}
	for name, workflow := range config.Workflows {
		requiresApproval := false
		usesSecret := false
		for _, step := range workflow.Steps {
			operation := config.Capabilities[step.Use]
			if operationRequiresApproval(operation) {
				requiresApproval = true
			}
			usesSecret = usesSecret || operationUsesSecret(config, operation) || hasSecretTemplate(step.Input)
		}
		result = append(result, makeCapability(config, name, "workflow", workflow.Description, workflow.InputSchema, workflow.OutputSchema, requiresApproval, usesSecret))
	}
	return result, nil
}

func makeCapability(config *Config, name, kind, description string, input, output map[string]any, requiresApproval, usesSecret bool) capability.Capability {
	if description == "" {
		description = config.Description
	}
	if description == "" {
		description = name + " from " + config.Name
	}
	if input == nil {
		input = map[string]any{"type": "object"}
	}
	if output == nil {
		output = map[string]any{}
	}
	metadata, _ := json.Marshal(map[string]any{"sourceDigest": config.Digest, "sourceKind": "declarative"})
	approval := "never"
	if requiresApproval {
		approval = "always"
	}
	upstreamAuth := "none"
	if usesSecret {
		upstreamAuth = "secret"
	}
	return capability.Capability{
		ID:          capability.StableID("api", config.Name, name),
		Kind:        kind,
		Name:        name,
		Description: description,
		Source: capability.Source{
			Protocol:     "api",
			Provider:     config.Name,
			UpstreamName: name,
		},
		Input:       capability.Contract{Mode: "json", JSONSchema: input, MediaTypes: []string{"application/json"}},
		Output:      capability.Contract{Mode: "json", JSONSchema: output, MediaTypes: []string{"application/json"}},
		Lifecycle:   capability.Lifecycle{Sync: true, Cancelable: true},
		Security:    capability.Security{RequiresApproval: approval, UpstreamAuth: upstreamAuth},
		RawMetadata: metadata,
		Revision:    1,
	}
}

func operationUsesSecret(config *Config, operation Operation) bool {
	if hasSecretTemplate(config.Services[operation.Service].Headers) || hasSecretTemplate(operation.Request) {
		return true
	}
	for _, hook := range append(append([]Hook(nil), operation.Hooks.BeforeRequest...), operation.Hooks.AfterResponse...) {
		if hasSecretTemplate(hook.SetHeaders) {
			return true
		}
	}
	return false
}

func hasSecretTemplate(value any) bool {
	found := false
	err := walkStrings(value, func(text string) error {
		for _, match := range templatePattern.FindAllStringSubmatch(text, -1) {
			found = found || match[1] == "secrets"
		}
		return nil
	})
	return err == nil && found
}

func operationRequiresApproval(operation Operation) bool {
	if operation.RequiresApproval {
		return true
	}
	if !strings.EqualFold(operation.Method, "GET") {
		return true
	}
	for _, hook := range append(append([]Hook(nil), operation.Hooks.BeforeRequest...), operation.Hooks.AfterResponse...) {
		if hook.Exec != nil {
			return true
		}
	}
	return false
}

func (a *Adapter) Invoke(ctx context.Context, p provider.Provider, c capability.Capability, _ string, input json.RawMessage, sink provider.Sink) error {
	config, err := a.source(p)
	if err != nil {
		return err
	}
	var data any
	if err := jsonvalue.Decode(input, &data); err != nil {
		return fmt.Errorf("decode input: %w", err)
	}
	credentials := newCredentialValues(secret.WithWorkspace(ctx, p.Config["workspace_root"]), a.resolver, config.Credentials)
	var result any
	if operation, ok := config.Capabilities[c.Source.UpstreamName]; ok {
		result, err = invokeOperation(ctx, operation, config.Services[operation.Service], data, credentials)
	} else if workflow, ok := config.Workflows[c.Source.UpstreamName]; ok {
		result, err = invokeWorkflow(ctx, config, workflow, data, credentials)
	} else {
		return fmt.Errorf("capability %q is not defined by source", c.ID)
	}
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if err := sink.Started(); err != nil {
		return err
	}
	return sink.Event(provider.Event{Type: "result", Data: encoded})
}

func (*Adapter) Cancel(context.Context, provider.Provider, string) error { return nil }

func (a *Adapter) Health(_ context.Context, p provider.Provider) provider.Health {
	_, err := a.source(p)
	return provider.Health{Healthy: err == nil, Message: healthMessage(err)}
}

func healthMessage(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ready"
}

func (a *Adapter) Close(_ context.Context, p provider.Provider) error {
	a.Unregister(p.ID)
	return nil
}
