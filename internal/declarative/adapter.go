package declarative

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
)

type Adapter struct {
	mu      sync.RWMutex
	sources map[string]*Config
}

func NewAdapter() *Adapter {
	return &Adapter{sources: make(map[string]*Config)}
}

func (a *Adapter) Register(p provider.Provider, config *Config) error {
	if config == nil {
		return errors.New("declarative config is nil")
	}
	if p.Protocol != "api" || p.Name != config.Metadata.Name {
		return errors.New("provider identity does not match declarative source")
	}
	a.mu.Lock()
	a.sources[p.ID] = config
	a.mu.Unlock()
	return nil
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
		return nil, fmt.Errorf("declarative source %q is not registered", p.Name)
	}
	return config, nil
}

func (a *Adapter) Discover(_ context.Context, p provider.Provider) ([]capability.Capability, error) {
	config, err := a.source(p)
	if err != nil {
		return nil, err
	}
	result := make([]capability.Capability, 0, len(config.Operations)+len(config.Workflows))
	for name, operation := range config.Operations {
		result = append(result, makeCapability(config, name, "operation", operation.Description, operation.InputSchema, operation.OutputSchema, operationRequiresApproval(operation)))
	}
	for name, workflow := range config.Workflows {
		requiresApproval := false
		for _, step := range workflow.Steps {
			if operationRequiresApproval(config.Operations[step.Use]) {
				requiresApproval = true
				break
			}
		}
		result = append(result, makeCapability(config, name, "workflow", workflow.Description, workflow.InputSchema, workflow.OutputSchema, requiresApproval))
	}
	return result, nil
}

func makeCapability(config *Config, name, kind, description string, input, output map[string]any, requiresApproval bool) capability.Capability {
	if description == "" {
		description = config.Metadata.Description
	}
	if description == "" {
		description = name + " from " + config.Metadata.Name
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
	return capability.Capability{
		ID:          capability.StableID("api", config.Metadata.Name, name),
		Kind:        kind,
		Name:        name,
		Description: description,
		Source: capability.Source{
			Protocol:     "api",
			Provider:     config.Metadata.Name,
			UpstreamName: name,
		},
		Input:       capability.Contract{Mode: "json", JSONSchema: input, MediaTypes: []string{"application/json"}},
		Output:      capability.Contract{Mode: "json", JSONSchema: output, MediaTypes: []string{"application/json"}},
		Lifecycle:   capability.Lifecycle{Sync: true, Cancelable: true},
		Security:    capability.Security{RequiresApproval: approval, UpstreamAuth: "environment"},
		RawMetadata: metadata,
		Revision:    1,
	}
}

func operationRequiresApproval(operation Operation) bool {
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
	if err := json.Unmarshal(input, &data); err != nil {
		return fmt.Errorf("decode input: %w", err)
	}
	variables, err := loadVariables(config.Variables)
	if err != nil {
		return err
	}
	var result any
	if operation, ok := config.Operations[c.Source.UpstreamName]; ok {
		result, err = invokeOperation(ctx, operation, config.Services[operation.Service], data, variables)
	} else if workflow, ok := config.Workflows[c.Source.UpstreamName]; ok {
		result, err = invokeWorkflow(ctx, config, workflow, data, variables)
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
