package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/workspace"
	"golang.org/x/sys/unix"
)

type IntegrationResult struct {
	Name         string   `json:"name"`
	Source       string   `json:"source"`
	Capabilities []string `json:"capabilities"`
}

func (a *App) Connect(ctx context.Context, identity string, source []byte, workspaceRoot string) (IntegrationResult, error) {
	if !a.IsAdmin(ctx, identity) {
		return IntegrationResult{}, errors.New("admin permission required")
	}
	var err error
	workspaceRoot, err = canonicalWorkspaceRoot(workspaceRoot)
	if err != nil {
		return IntegrationResult{}, err
	}
	config, err := declarative.Parse(source)
	if err != nil {
		return IntegrationResult{}, err
	}
	p, err := integrationProvider(config, workspaceRoot)
	if err != nil {
		return IntegrationResult{}, err
	}
	a.mu.RLock()
	adapter := a.adapters[p.Protocol]
	a.mu.RUnlock()
	if adapter == nil {
		return IntegrationResult{}, fmt.Errorf("%s integration runtime is unavailable", config.Type)
	}
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return IntegrationResult{}, err
	}
	defer lease.done()
	a.mutation.Lock()
	defer a.mutation.Unlock()
	gate := a.providerGate(p.ID)
	gate.Lock()
	defer gate.Unlock()
	if err := lease.check(); err != nil {
		return IntegrationResult{}, err
	}
	previous, err := a.integrationByName(lease.ctx, filepath.Clean(workspaceRoot), config.Name)
	if err != nil {
		return IntegrationResult{}, err
	}
	var previousConfig *declarative.Config
	if previous != nil {
		if previous.Protocol != p.Protocol {
			return IntegrationResult{}, fmt.Errorf("integration %q is already connected as %s; disconnect it before changing type", config.Name, integrationType(previous.Protocol))
		}
		previousConfig, err = integrationConfig(*previous)
		if err != nil {
			return IntegrationResult{}, err
		}
	}
	if err := lease.setTarget(adapter, p); err != nil {
		return IntegrationResult{}, err
	}
	sourcePath := declarativeSourcePath(workspaceRoot, config.Name)
	previousSource, sourceExisted, err := readDeclarativeSource(sourcePath)
	if err != nil {
		return IntegrationResult{}, fmt.Errorf("read integration source %q: %w", config.Name, err)
	}
	if err := writeDeclarativeSource(sourcePath, source); err != nil {
		restoreErr := restoreDeclarativeSource(sourcePath, previousSource, sourceExisted)
		return IntegrationResult{}, errors.Join(err, restoreErr)
	}
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(lease.ctx), 30*time.Second)
	defer cancelCleanup()
	if httpAdapter, ok := adapter.(*declarative.Adapter); ok {
		if err := httpAdapter.Register(p, config); err != nil {
			return IntegrationResult{}, errors.Join(err, restoreDeclarativeSource(sourcePath, previousSource, sourceExisted))
		}
	}
	capabilities, err := adapter.Discover(lease.ctx, p)
	if err != nil {
		registrationErr := a.restoreIntegrationAdapter(cleanupCtx, adapter, p, previous, previousConfig)
		return IntegrationResult{}, errors.Join(err, registrationErr, restoreDeclarativeSource(sourcePath, previousSource, sourceExisted))
	}
	capabilities = scopeIntegrationCapabilities(p, capabilities)
	grants, err := a.grantIntegrationCapabilities(lease.ctx, cleanupCtx, identity, capabilities)
	if err != nil {
		registrationErr := a.restoreIntegrationAdapter(cleanupCtx, adapter, p, previous, previousConfig)
		return IntegrationResult{}, errors.Join(err, registrationErr, restoreDeclarativeSource(sourcePath, previousSource, sourceExisted))
	}
	if _, err := a.projections.Attach(lease.ctx, workspaceRoot, workspace.PolicyAuto); err != nil {
		revokeErr := a.revokeIntegrationGrants(cleanupCtx, identity, grants)
		registrationErr := a.restoreIntegrationAdapter(cleanupCtx, adapter, p, previous, previousConfig)
		return IntegrationResult{}, errors.Join(err, revokeErr, registrationErr, restoreDeclarativeSource(sourcePath, previousSource, sourceExisted))
	}
	if _, err := a.cat.ReplaceProviderCapabilities(lease.ctx, p, capabilities); err != nil {
		revokeErr := a.revokeIntegrationGrants(cleanupCtx, identity, grants)
		registrationErr := a.restoreIntegrationAdapter(cleanupCtx, adapter, p, previous, previousConfig)
		sourceErr := restoreDeclarativeSource(sourcePath, previousSource, sourceExisted)
		return IntegrationResult{}, lease.result(errors.Join(err, revokeErr, registrationErr, sourceErr))
	}
	a.mu.Lock()
	if a.state == appOpen {
		a.providers[p.ID] = p
	}
	a.mu.Unlock()
	ids := make([]string, 0, len(capabilities))
	for _, item := range capabilities {
		ids = append(ids, publicCapabilityRef(item))
	}
	sort.Strings(ids)
	return IntegrationResult{
		Name:         config.Name,
		Source:       filepath.ToSlash(filepath.Join(".9a", "integrations", config.Name+".yaml")),
		Capabilities: ids,
	}, nil
}

type declarativeGrant struct {
	capability string
	permission authz.Permission
}

func (a *App) grantIntegrationCapabilities(ctx, cleanupCtx context.Context, identity string, capabilities []capability.Capability) ([]declarativeGrant, error) {
	var added []declarativeGrant
	for _, item := range capabilities {
		for _, permission := range []authz.Permission{authz.Read, authz.Invoke} {
			created, err := a.az.GrantIfAbsent(ctx, identity, item.ID, permission)
			if err != nil {
				revokeErr := a.revokeIntegrationGrants(cleanupCtx, identity, added)
				return nil, errors.Join(err, revokeErr)
			}
			if created {
				added = append(added, declarativeGrant{item.ID, permission})
			}
		}
	}
	return added, nil
}

func (a *App) revokeIntegrationGrants(ctx context.Context, identity string, grants []declarativeGrant) error {
	var result error
	for _, grant := range grants {
		result = errors.Join(result, a.az.Revoke(ctx, identity, grant.capability, grant.permission))
	}
	return result
}

func (a *App) DisconnectFromWorkspace(ctx context.Context, identity, root, name string) error {
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return err
	}
	return a.disconnect(ctx, identity, canonical, name)
}

func (a *App) disconnect(ctx context.Context, identity, root, name string) error {
	if !a.IsAdmin(ctx, identity) {
		return errors.New("admin permission required")
	}
	if name == "" || capability.Slug(name) != name {
		return errors.New("integration must be a canonical non-empty slug")
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
	p, err := a.integrationByName(lease.ctx, root, name)
	if err != nil {
		return err
	}
	if p == nil {
		return fmt.Errorf("integration %q not found", name)
	}
	gate := a.providerGate(p.ID)
	gate.Lock()
	defer gate.Unlock()
	a.mu.RLock()
	adapter := a.adapters[p.Protocol]
	a.mu.RUnlock()
	if err := catalog.New(a.db).DeleteProvider(lease.ctx, p.ID); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.providers, p.ID)
	a.mu.Unlock()
	if adapter != nil {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(lease.ctx), 30*time.Second)
		defer cancel()
		if err := adapter.Close(cleanupCtx, *p); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) integrationByName(ctx context.Context, root, name string) (*provider.Provider, error) {
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	root = canonical
	providers, err := a.cat.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	var match *provider.Provider
	for _, item := range providers {
		if item.Name == name && (item.Protocol == "api" || item.Protocol == "mcp" || item.Protocol == "a2a") {
			itemRoot, rootErr := integrationWorkspaceRoot(item)
			if rootErr != nil || filepath.Clean(itemRoot) != root {
				continue
			}
			if match != nil {
				return nil, fmt.Errorf("integration name %q is ambiguous", name)
			}
			copy := item
			match = &copy
		}
	}
	return match, nil
}

func restoreDeclarativeRegistration(adapter *declarative.Adapter, previous *provider.Provider, config *declarative.Config, providerID string) error {
	if previous == nil {
		adapter.Unregister(providerID)
		return nil
	}
	if config == nil {
		return errors.New("previous integration config is unavailable")
	}
	return adapter.Register(*previous, config)
}

func (a *App) restoreIntegrationAdapter(ctx context.Context, adapter provider.Adapter, current provider.Provider, previous *provider.Provider, previousConfig *declarative.Config) error {
	if httpAdapter, ok := adapter.(*declarative.Adapter); ok {
		return restoreDeclarativeRegistration(httpAdapter, previous, previousConfig, current.ID)
	}
	result := adapter.Close(ctx, current)
	if previous != nil && current.Protocol == "a2a" {
		_, err := adapter.Discover(ctx, *previous)
		result = errors.Join(result, err)
	}
	return result
}

func integrationProvider(config *declarative.Config, workspaceRoot string) (provider.Provider, error) {
	protocol := ""
	endpoint := ""
	switch config.Type {
	case "http":
		protocol = "api"
		endpoint = "declarative://" + config.Name
	case "mcp":
		protocol = "mcp"
		endpoint = "stdio:" + config.Executable
	case "a2a":
		protocol = "a2a"
		endpoint = config.URL
	default:
		return provider.Provider{}, fmt.Errorf("unsupported integration type %q", config.Type)
	}
	providerConfig := map[string]string{"workspace_root": workspaceRoot}
	if config.Type == "a2a" {
		for _, credential := range config.Credentials {
			providerConfig["credential_reference"] = credential.Secret
		}
	}
	return provider.Provider{
		ID:       protocol + "/" + workspace.StableID(workspaceRoot) + "/" + config.Name,
		Protocol: protocol,
		Name:     config.Name,
		Endpoint: endpoint,
		Config:   providerConfig,
	}, nil
}

func integrationType(protocol string) string {
	if protocol == "api" {
		return "http"
	}
	return protocol
}

func sameIntegrationProvider(left, right provider.Provider) bool {
	return left.ID == right.ID && left.Protocol == right.Protocol && left.Name == right.Name && left.Endpoint == right.Endpoint && reflectStringMap(left.Config, right.Config)
}

func reflectStringMap(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func declarativeSourcePath(workspaceRoot, name string) string {
	return filepath.Join(workspaceRoot, ".9a", "integrations", name+".yaml")
}

func readDeclarativeSource(path string) ([]byte, bool, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("open canonical source without following links: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, false, errors.New("open canonical source")
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, false, fmt.Errorf("inspect canonical source: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, false, errors.New("canonical source must be a regular file")
	}
	if info.Size() > declarative.MaxSourceBytes {
		_ = file.Close()
		return nil, false, fmt.Errorf("canonical source exceeds %d bytes", declarative.MaxSourceBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, declarative.MaxSourceBytes+1))
	closeErr := file.Close()
	if err != nil {
		return nil, false, fmt.Errorf("read canonical source: %w", err)
	}
	if closeErr != nil {
		return nil, false, fmt.Errorf("close canonical source: %w", closeErr)
	}
	if len(data) > declarative.MaxSourceBytes {
		return nil, false, fmt.Errorf("canonical source exceeds %d bytes", declarative.MaxSourceBytes)
	}
	return data, true, nil
}

func integrationWorkspaceRoot(p provider.Provider) (string, error) {
	root := p.Config["workspace_root"]
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("integration %q has invalid workspace root", p.Name)
	}
	return root, nil
}

func integrationConfig(p provider.Provider) (*declarative.Config, error) {
	root, err := integrationWorkspaceRoot(p)
	if err != nil {
		return nil, err
	}
	if p.Name == "" || p.Name == "." || p.Name == ".." || filepath.Base(p.Name) != p.Name {
		return nil, fmt.Errorf("integration has invalid name %q", p.Name)
	}
	path := declarativeSourcePath(root, p.Name)
	source, exists, err := readDeclarativeSource(path)
	if err != nil {
		return nil, fmt.Errorf("read integration source %q: %w", p.Name, err)
	}
	if !exists {
		return nil, fmt.Errorf("read integration source %q: %w", p.Name, os.ErrNotExist)
	}
	config, err := declarative.Parse(source)
	if err != nil {
		return nil, fmt.Errorf("parse integration source %q: %w", p.Name, err)
	}
	if config.Name != p.Name {
		return nil, fmt.Errorf("integration source %q defines name %q", p.Name, config.Name)
	}
	desired, err := integrationProvider(config, root)
	if err != nil {
		return nil, err
	}
	if desired.Protocol != p.Protocol {
		return nil, fmt.Errorf("integration source %q changed type; disconnect it before reconnecting", p.Name)
	}
	return config, nil
}

func writeDeclarativeSource(path string, source []byte) (result error) {
	directory := filepath.Dir(path)
	if err := ensureIntegrationSourceDirectory(directory); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".source-*.tmp")
	if err != nil {
		return fmt.Errorf("create integration source: %w", err)
	}
	temporary := file.Name()
	defer func() {
		if file != nil {
			result = errors.Join(result, file.Close())
		}
		if temporary != "" {
			removeErr := os.Remove(temporary)
			if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
				result = errors.Join(result, removeErr)
			}
		}
	}()
	if err := file.Chmod(0o644); err != nil {
		return fmt.Errorf("set integration source permissions: %w", err)
	}
	if _, err := file.Write(source); err != nil {
		return fmt.Errorf("write integration source: %w", err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync integration source: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close integration source: %w", err)
	}
	file = nil
	if err := os.Rename(temporary, path); err != nil {
		return fmt.Errorf("replace integration source: %w", err)
	}
	temporary = ""
	dir, err := os.Open(directory)
	if err != nil {
		return fmt.Errorf("open integration source directory: %w", err)
	}
	if err := dir.Sync(); err != nil {
		closeErr := dir.Close()
		return errors.Join(fmt.Errorf("sync integration source directory: %w", err), closeErr)
	}
	if err := dir.Close(); err != nil {
		return fmt.Errorf("close integration source directory: %w", err)
	}
	return nil
}

func ensureIntegrationSourceDirectory(directory string) error {
	parent := filepath.Dir(directory)
	for _, path := range []string{parent, directory} {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create integration source directory: %w", err)
			}
			info, err = os.Lstat(path)
		}
		if err != nil {
			return fmt.Errorf("inspect integration source directory: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("integration source directory %q must be a real directory", path)
		}
	}
	return nil
}

func restoreDeclarativeSource(path string, previous []byte, existed bool) error {
	if existed {
		return writeDeclarativeSource(path, previous)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove integration source: %w", err)
	}
	return nil
}
