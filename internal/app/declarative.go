package app

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"time"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/mount/dir"
	"github.com/gopact-ai/9a/internal/provider"
)

type DeclarativeResult struct {
	Name         string   `json:"name"`
	Digest       string   `json:"digest"`
	Root         string   `json:"root"`
	Capabilities []string `json:"capabilities"`
}

type DeclarativeDiff struct {
	Name     string   `json:"name"`
	Changed  bool     `json:"changed"`
	Added    []string `json:"added,omitempty"`
	Removed  []string `json:"removed,omitempty"`
	Modified []string `json:"modified,omitempty"`
}

func (a *App) AddDeclarative(ctx context.Context, identity string, source []byte, workspaceRoot string) (DeclarativeResult, error) {
	if !a.IsAdmin(ctx, identity) {
		return DeclarativeResult{}, errors.New("admin permission required")
	}
	if !filepath.IsAbs(workspaceRoot) {
		return DeclarativeResult{}, errors.New("workspace root must be absolute")
	}
	config, err := declarative.Parse(source)
	if err != nil {
		return DeclarativeResult{}, err
	}
	skill, err := declarative.RenderSkill(config)
	if err != nil {
		return DeclarativeResult{}, err
	}
	projectionRoot := filepath.Join(workspaceRoot, filepath.FromSlash(config.SkillRoot()))
	p := provider.Provider{
		ID:       "api/" + config.Metadata.Name,
		Protocol: "api",
		Name:     config.Metadata.Name,
		Endpoint: "declarative://" + config.Metadata.Name,
		Config:   map[string]string{"source": string(source), "projection_root": projectionRoot},
	}
	a.mu.RLock()
	adapter, ok := a.adapters["api"].(*declarative.Adapter)
	a.mu.RUnlock()
	if !ok {
		return DeclarativeResult{}, errors.New("declarative adapter is unavailable")
	}
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return DeclarativeResult{}, err
	}
	defer lease.done()
	a.mutation.Lock()
	defer a.mutation.Unlock()
	if err := lease.check(); err != nil {
		return DeclarativeResult{}, err
	}
	previous, err := a.declarativeProvider(lease.ctx, p.ID)
	if err != nil {
		return DeclarativeResult{}, err
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(lease.ctx), 30*time.Second)
	defer cancelCleanup()
	if err := adapter.Register(p, config); err != nil {
		return DeclarativeResult{}, err
	}
	capabilities, err := adapter.Discover(lease.ctx, p)
	if err != nil {
		restoreDeclarativeRegistration(adapter, previous, p.ID)
		return DeclarativeResult{}, err
	}
	grants, err := a.grantDeclarativeCapabilities(lease.ctx, cleanupCtx, identity, capabilities)
	if err != nil {
		restoreDeclarativeRegistration(adapter, previous, p.ID)
		return DeclarativeResult{}, err
	}
	backend := dir.New()
	if err := backend.Publish(lease.ctx, projectionRoot, skill); err != nil {
		revokeErr := a.revokeDeclarativeGrants(cleanupCtx, identity, grants)
		restoreDeclarativeRegistration(adapter, previous, p.ID)
		return DeclarativeResult{}, errors.Join(err, revokeErr)
	}
	oldProjectionRemoved := false
	if previous != nil && previous.Config["projection_root"] != projectionRoot {
		oldConfig, parseErr := declarative.Parse([]byte(previous.Config["source"]))
		if parseErr != nil {
			_ = backend.Remove(cleanupCtx, projectionRoot, skill)
			revokeErr := a.revokeDeclarativeGrants(cleanupCtx, identity, grants)
			restoreDeclarativeRegistration(adapter, previous, p.ID)
			return DeclarativeResult{}, errors.Join(parseErr, revokeErr)
		}
		oldSkill, renderErr := declarative.RenderSkill(oldConfig)
		if renderErr != nil {
			_ = backend.Remove(cleanupCtx, projectionRoot, skill)
			revokeErr := a.revokeDeclarativeGrants(cleanupCtx, identity, grants)
			restoreDeclarativeRegistration(adapter, previous, p.ID)
			return DeclarativeResult{}, errors.Join(renderErr, revokeErr)
		}
		if removeErr := backend.Remove(lease.ctx, previous.Config["projection_root"], oldSkill); removeErr != nil {
			_ = backend.Remove(cleanupCtx, projectionRoot, skill)
			revokeErr := a.revokeDeclarativeGrants(cleanupCtx, identity, grants)
			restoreDeclarativeRegistration(adapter, previous, p.ID)
			return DeclarativeResult{}, errors.Join(removeErr, revokeErr)
		}
		oldProjectionRemoved = true
	}
	if err := a.addProviderLocked(lease, p); err != nil {
		removeErr := backend.Remove(cleanupCtx, projectionRoot, skill)
		var restoreErr error
		if previous != nil && (oldProjectionRemoved || previous.Config["projection_root"] == projectionRoot) {
			restoreErr = restoreDeclarativeProjection(cleanupCtx, backend, previous)
		}
		revokeErr := a.revokeDeclarativeGrants(cleanupCtx, identity, grants)
		restoreDeclarativeRegistration(adapter, previous, p.ID)
		return DeclarativeResult{}, errors.Join(err, removeErr, restoreErr, revokeErr)
	}
	ids := make([]string, 0, len(capabilities))
	for _, item := range capabilities {
		ids = append(ids, item.ID)
	}
	sort.Strings(ids)
	return DeclarativeResult{Name: config.Metadata.Name, Digest: config.Digest, Root: filepath.Join(projectionRoot, config.Metadata.Name), Capabilities: ids}, nil
}

type declarativeGrant struct {
	capability string
	permission authz.Permission
}

func (a *App) grantDeclarativeCapabilities(ctx, cleanupCtx context.Context, identity string, capabilities []capability.Capability) ([]declarativeGrant, error) {
	var added []declarativeGrant
	for _, item := range capabilities {
		for _, permission := range []authz.Permission{authz.Read, authz.Invoke} {
			created, err := a.az.GrantIfAbsent(ctx, identity, item.ID, permission)
			if err != nil {
				revokeErr := a.revokeDeclarativeGrants(cleanupCtx, identity, added)
				return nil, errors.Join(err, revokeErr)
			}
			if created {
				added = append(added, declarativeGrant{item.ID, permission})
			}
		}
	}
	return added, nil
}

func (a *App) revokeDeclarativeGrants(ctx context.Context, identity string, grants []declarativeGrant) error {
	var result error
	for _, grant := range grants {
		result = errors.Join(result, a.az.Revoke(ctx, identity, grant.capability, grant.permission))
	}
	return result
}

func (a *App) DiffDeclarative(ctx context.Context, source []byte) (DeclarativeDiff, error) {
	config, err := declarative.Parse(source)
	if err != nil {
		return DeclarativeDiff{}, err
	}
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return DeclarativeDiff{}, err
	}
	defer lease.done()
	a.mutation.RLock()
	defer a.mutation.RUnlock()
	p, err := a.declarativeProvider(lease.ctx, "api/"+config.Metadata.Name)
	if err != nil {
		return DeclarativeDiff{}, err
	}
	if p == nil {
		return DeclarativeDiff{Name: config.Metadata.Name, Changed: true, Added: declarativeKeys(config)}, nil
	}
	current, err := declarative.Parse([]byte(p.Config["source"]))
	if err != nil {
		return DeclarativeDiff{}, err
	}
	diff := DeclarativeDiff{Name: config.Metadata.Name, Changed: current.Digest != config.Digest}
	currentValues := declarativeValues(current)
	nextValues := declarativeValues(config)
	for name, value := range nextValues {
		old, exists := currentValues[name]
		if !exists {
			diff.Added = append(diff.Added, name)
		} else if !reflect.DeepEqual(old, value) {
			diff.Modified = append(diff.Modified, name)
		}
	}
	for name := range currentValues {
		if _, exists := nextValues[name]; !exists {
			diff.Removed = append(diff.Removed, name)
		}
	}
	sort.Strings(diff.Added)
	sort.Strings(diff.Removed)
	sort.Strings(diff.Modified)
	return diff, nil
}

func (a *App) RemoveDeclarative(ctx context.Context, identity, name string) error {
	if !a.IsAdmin(ctx, identity) {
		return errors.New("admin permission required")
	}
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return err
	}
	defer lease.done()
	a.mutation.Lock()
	defer a.mutation.Unlock()
	if err := lease.check(); err != nil {
		return err
	}
	p, err := a.declarativeProvider(lease.ctx, "api/"+name)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("declarative source %q not found", name)
	}
	config, err := declarative.Parse([]byte(p.Config["source"]))
	if err != nil {
		return err
	}
	skill, err := declarative.RenderSkill(config)
	if err != nil {
		return err
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(lease.ctx), 30*time.Second)
	defer cancelCleanup()
	backend := dir.New()
	if err := backend.Remove(lease.ctx, p.Config["projection_root"], skill); err != nil {
		return err
	}
	if err := catalog.New(a.db).DeleteProvider(lease.ctx, p.ID); err != nil {
		return errors.Join(err, backend.Publish(cleanupCtx, p.Config["projection_root"], skill))
	}
	a.mu.Lock()
	delete(a.providers, p.ID)
	adapter, _ := a.adapters["api"].(*declarative.Adapter)
	a.mu.Unlock()
	if adapter != nil {
		adapter.Unregister(p.ID)
	}
	return nil
}

func (a *App) declarativeProvider(ctx context.Context, id string) (*provider.Provider, error) {
	providers, err := a.cat.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, item := range providers {
		if item.ID == id && item.Protocol == "api" {
			copy := item
			return &copy, nil
		}
	}
	return nil, nil
}

func restoreDeclarativeRegistration(adapter *declarative.Adapter, previous *provider.Provider, providerID string) {
	if previous == nil {
		adapter.Unregister(providerID)
		return
	}
	if config, err := declarative.Parse([]byte(previous.Config["source"])); err == nil {
		_ = adapter.Register(*previous, config)
	}
}

func restoreDeclarativeProjection(ctx context.Context, backend *dir.Backend, previous *provider.Provider) error {
	if previous == nil {
		return nil
	}
	config, err := declarative.Parse([]byte(previous.Config["source"]))
	if err != nil {
		return err
	}
	skill, err := declarative.RenderSkill(config)
	if err != nil {
		return err
	}
	return backend.Publish(ctx, previous.Config["projection_root"], skill)
}

func declarativeValues(config *declarative.Config) map[string]any {
	values := make(map[string]any, len(config.Operations)+len(config.Workflows))
	for name, value := range config.Operations {
		values["operation/"+name] = value
	}
	for name, value := range config.Workflows {
		values["workflow/"+name] = value
	}
	return values
}

func declarativeKeys(config *declarative.Config) []string {
	values := declarativeValues(config)
	result := make([]string, 0, len(values))
	for name := range values {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}
