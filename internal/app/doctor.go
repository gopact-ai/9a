package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/workspace"
)

type DoctorCheck struct {
	Name       string `json:"name"`
	State      string `json:"state"`
	Message    string `json:"message"`
	NextAction string `json:"nextAction,omitempty"`
}

type DoctorReport struct {
	Healthy bool          `json:"healthy"`
	Fixed   int           `json:"fixed"`
	Checks  []DoctorCheck `json:"checks"`
}

func (a *App) Doctor(ctx context.Context, identity, root string, fix bool) (DoctorReport, error) {
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return DoctorReport{}, err
	}
	root = canonical
	if fix && !a.IsAdmin(ctx, identity) {
		return DoctorReport{}, errors.New("admin permission required")
	}
	report := DoctorReport{Healthy: true, Checks: []DoctorCheck{{Name: "runtime", State: "ok", Message: "local runtime responded"}}}

	status, err := a.workspaceStatus(ctx, root)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("inspect workspace: %w", err)
	}
	if status.Workspace.State != workspace.StateHealthy {
		if fix {
			if _, err := a.projections.Attach(ctx, root, workspace.PolicyAuto); err != nil {
				report.addProblem("workspace", "gateway view could not be repaired", "Move the conflicting .agents/skills/using-ninea directory, then run 9a doctor --fix")
			} else {
				report.addFixed("workspace", "gateway view is ready")
			}
		} else {
			report.addProblem("workspace", "gateway view is missing or modified", "9a doctor --fix")
		}
	} else {
		report.Checks = append(report.Checks, DoctorCheck{Name: "workspace", State: "ok", Message: "gateway view is ready"})
	}

	providers, err := a.cat.ListProviders(ctx)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("list integrations: %w", err)
	}
	capabilities, err := catalog.New(a.db).ListCapabilities(ctx)
	if err != nil {
		return DoctorReport{}, fmt.Errorf("list capabilities: %w", err)
	}
	byProvider := make(map[string][]capability.Capability)
	for _, item := range capabilities {
		providerID := capabilityProviderID(item)
		byProvider[providerID] = append(byProvider[providerID], item)
	}
	active := 0
	for _, p := range providers {
		if p.Protocol != "api" && p.Protocol != "mcp" && p.Protocol != "a2a" {
			continue
		}
		workspaceRoot, rootErr := integrationWorkspaceRoot(p)
		if rootErr != nil || filepath.Clean(workspaceRoot) != filepath.Clean(root) {
			continue
		}
		active++
		config, configErr := integrationConfig(p)
		if configErr != nil {
			report.addProblem("integration/"+p.Name, "canonical source is invalid: "+configErr.Error(), "Edit .9a/integrations/"+p.Name+".yaml, then run 9a connect .9a/integrations/"+p.Name+".yaml")
			continue
		}
		desiredProvider, providerErr := integrationProvider(config, workspaceRoot)
		if providerErr != nil {
			return DoctorReport{}, providerErr
		}
		current := byProvider[p.ID]
		stale := !sameIntegrationProvider(p, desiredProvider) || len(current) == 0
		if p.Protocol == "api" {
			desiredAdapter := declarative.NewAdapter()
			if registerErr := desiredAdapter.Register(desiredProvider, config); registerErr != nil {
				return DoctorReport{}, registerErr
			}
			desired, discoverErr := desiredAdapter.Discover(ctx, desiredProvider)
			if discoverErr != nil {
				return DoctorReport{}, discoverErr
			}
			desired = scopeIntegrationCapabilities(desiredProvider, desired)
			a.mu.RLock()
			liveAdapter, _ := a.adapters["api"].(*declarative.Adapter)
			a.mu.RUnlock()
			var live *declarative.Config
			if liveAdapter != nil {
				live = liveAdapter.Snapshot(p.ID)
			}
			stale = stale || live == nil || live.Digest != config.Digest || !sameCapabilities(current, desired)
		}
		if !stale {
			report.Checks = append(report.Checks, DoctorCheck{Name: "integration/" + p.Name, State: "ok", Message: "source and runtime agree"})
			continue
		}
		if !fix {
			report.addProblem("integration/"+p.Name, "source and runtime differ", "9a doctor --fix")
			continue
		}
		if _, err := a.Connect(ctx, identity, config.Source, root); err != nil {
			report.addProblem("integration/"+p.Name, "source could not be reconnected", "9a connect .9a/integrations/"+p.Name+".yaml")
			continue
		}
		report.addFixed("integration/"+p.Name, "runtime rebuilt from canonical source")
	}
	if active == 0 {
		report.Checks = append(report.Checks, DoctorCheck{Name: "integrations", State: "ok", Message: "no active integrations"})
	}
	return report, nil
}

func (r *DoctorReport) addProblem(name, message, next string) {
	r.Healthy = false
	r.Checks = append(r.Checks, DoctorCheck{Name: name, State: "problem", Message: message, NextAction: next})
}

func (r *DoctorReport) addFixed(name, message string) {
	r.Fixed++
	r.Checks = append(r.Checks, DoctorCheck{Name: name, State: "fixed", Message: message})
}
