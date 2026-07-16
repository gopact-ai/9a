package app

import (
	"context"
	"errors"
	"path/filepath"
	"sort"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/secret"
	"github.com/gopact-ai/9a/internal/workspace"
)

type IntegrationStatus struct {
	Name           string   `json:"name"`
	State          string   `json:"state"`
	Capabilities   int      `json:"capabilities"`
	MissingSecrets []string `json:"missingSecrets,omitempty"`
	Message        string   `json:"message,omitempty"`
}

type StatusResult struct {
	State        string              `json:"state"`
	Message      string              `json:"message,omitempty"`
	Integrations []IntegrationStatus `json:"integrations"`
}

func (a *App) Status(ctx context.Context, root, name string) (StatusResult, error) {
	if name != "" && capability.Slug(name) != name {
		return StatusResult{}, errors.New("integration must be a canonical non-empty slug")
	}
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return StatusResult{}, err
	}
	root = canonical
	workspaceStatus, err := a.workspaceStatus(ctx, root)
	if err != nil {
		return StatusResult{}, err
	}
	providers, err := a.cat.ListProviders(ctx)
	if err != nil {
		return StatusResult{}, err
	}
	capabilities, err := catalog.New(a.db).ListCapabilities(ctx)
	if err != nil {
		return StatusResult{}, err
	}
	capabilityCount := make(map[string]int)
	for _, item := range capabilities {
		capabilityCount[capabilityProviderID(item)]++
	}
	result := StatusResult{State: "ready", Integrations: []IntegrationStatus{}}
	for _, p := range providers {
		if p.Protocol != "api" && p.Protocol != "mcp" && p.Protocol != "a2a" {
			continue
		}
		workspaceRoot, rootErr := integrationWorkspaceRoot(p)
		if rootErr != nil || filepath.Clean(workspaceRoot) != filepath.Clean(root) || (name != "" && p.Name != name) {
			continue
		}
		status := IntegrationStatus{Name: p.Name, State: "ready", Capabilities: capabilityCount[p.ID]}
		if status.Capabilities == 0 {
			status.State = "broken"
			status.Message = "capability cache is empty"
		}
		config, configErr := integrationConfig(p)
		if configErr != nil {
			status.State = "broken"
			status.Message = "canonical source is invalid or missing"
			result.Integrations = append(result.Integrations, status)
			continue
		}
		desired, desiredErr := integrationProvider(config, workspaceRoot)
		if desiredErr != nil || !sameIntegrationProvider(p, desired) {
			status.State = "broken"
			status.Message = "source and runtime differ"
		}
		if p.Protocol == "api" {
			a.mu.RLock()
			adapter, _ := a.adapters["api"].(*declarative.Adapter)
			a.mu.RUnlock()
			var live *declarative.Config
			if adapter != nil {
				live = adapter.Snapshot(p.ID)
			}
			if live == nil || live.Digest != config.Digest {
				status.State = "broken"
				status.Message = "source and runtime differ"
			}
		}
		missing := map[string]struct{}{}
		secretCtx := secret.WithWorkspace(ctx, workspaceRoot)
		for _, credential := range config.Credentials {
			_, resolveErr := a.secrets.Resolve(secretCtx, credential.Secret)
			if errors.Is(resolveErr, secret.ErrMissing) {
				missing[credential.Secret] = struct{}{}
				continue
			}
			if resolveErr != nil {
				status.State = "broken"
				status.Message = "system credential store is unavailable"
			}
		}
		for reference := range missing {
			status.MissingSecrets = append(status.MissingSecrets, reference)
		}
		sort.Strings(status.MissingSecrets)
		if status.State == "ready" && len(status.MissingSecrets) > 0 {
			status.State = "needs-secret"
		}
		result.Integrations = append(result.Integrations, status)
	}
	sort.Slice(result.Integrations, func(i, j int) bool { return result.Integrations[i].Name < result.Integrations[j].Name })
	if name != "" && len(result.Integrations) == 0 {
		return StatusResult{}, errors.New("integration not found")
	}
	if len(result.Integrations) == 0 {
		result.State = "empty"
	} else if workspaceStatus.Workspace.State != workspace.StateHealthy {
		result.State = "broken"
		result.Message = "gateway is missing or modified"
	}
	for _, status := range result.Integrations {
		if status.State == "broken" {
			result.State = "broken"
			break
		}
		if status.State == "needs-secret" && result.State == "ready" {
			result.State = "needs-secret"
		}
	}
	return result, nil
}
