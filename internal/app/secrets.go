package app

import (
	"context"
	"errors"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/secret"
)

type SecretStatus struct {
	Name  string `json:"name"`
	State string `json:"state"`
}

func (a *App) SetSecret(ctx context.Context, identity, root, reference, value string) error {
	if !a.IsAdmin(ctx, identity) {
		return errors.New("admin permission required")
	}
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return err
	}
	return a.secrets.Set(secret.WithWorkspace(ctx, canonical), reference, value)
}

func (a *App) DeleteSecret(ctx context.Context, identity, root, reference string) error {
	if !a.IsAdmin(ctx, identity) {
		return errors.New("admin permission required")
	}
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return err
	}
	return a.secrets.Delete(secret.WithWorkspace(ctx, canonical), reference)
}

func (a *App) ListSecrets(ctx context.Context, root, integration string) ([]SecretStatus, error) {
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	root = canonical
	if integration != "" && capability.Slug(integration) != integration {
		return nil, errors.New("integration must be a canonical non-empty slug")
	}
	references := map[string]struct{}{}
	scopedCtx := secret.WithWorkspace(ctx, root)
	metadata, err := a.secrets.List(scopedCtx)
	if err != nil {
		return nil, err
	}
	for _, item := range metadata {
		references[item.Name] = struct{}{}
	}
	providers, err := a.cat.ListProviders(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range providers {
		if p.Protocol != "api" && p.Protocol != "mcp" && p.Protocol != "a2a" {
			continue
		}
		workspaceRoot, rootErr := integrationWorkspaceRoot(p)
		if rootErr != nil || filepath.Clean(workspaceRoot) != filepath.Clean(root) {
			continue
		}
		config, configErr := integrationConfig(p)
		if configErr != nil {
			continue
		}
		for _, credential := range config.Credentials {
			references[credential.Secret] = struct{}{}
		}
	}
	names := make([]string, 0, len(references))
	for reference := range references {
		owner, _, _ := strings.Cut(reference, ".")
		if integration == "" || owner == integration {
			names = append(names, reference)
		}
	}
	sort.Strings(names)
	result := make([]SecretStatus, 0, len(names))
	for _, reference := range names {
		_, resolveErr := a.secrets.Resolve(scopedCtx, reference)
		state := "present"
		if errors.Is(resolveErr, secret.ErrMissing) {
			state = "missing"
		} else if resolveErr != nil {
			return nil, resolveErr
		}
		result = append(result, SecretStatus{Name: reference, State: state})
	}
	return result, nil
}
