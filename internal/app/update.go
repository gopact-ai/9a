package app

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/generator"
	"github.com/gopact-ai/9a/internal/mount"
	"github.com/gopact-ai/9a/internal/provider"
)

type ProviderUpdate struct {
	ID    string `json:"id"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}
type WorkspaceUpdate struct {
	Root      string `json:"root"`
	Updated   int    `json:"updated"`
	Unchanged int    `json:"unchanged"`
	Repaired  int    `json:"repaired"`
	Removed   int    `json:"removed"`
	Failed    int    `json:"failed"`
}
type UpdateResult struct {
	Providers  []ProviderUpdate  `json:"providers"`
	Workspaces []WorkspaceUpdate `json:"workspaces"`
	Failed     int               `json:"failed"`
}

func (a *App) UpdateWorkspaces(ctx context.Context, root string, check, all bool) (UpdateResult, error) {
	lease, err := a.beginOperation(ctx)
	if err != nil {
		return UpdateResult{}, err
	}
	defer lease.done()
	a.mutation.Lock()
	defer a.mutation.Unlock()
	result := UpdateResult{}
	a.mu.RLock()
	ids := make([]string, 0, len(a.providers))
	for id := range a.providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	entries := make([]struct {
		p  provider.Provider
		ad provider.Adapter
	}, 0, len(ids))
	for _, id := range ids {
		p := a.providers[id]
		entries = append(entries, struct {
			p  provider.Provider
			ad provider.Adapter
		}{p, a.adapters[p.Protocol]})
	}
	a.mu.RUnlock()
	for _, entry := range entries {
		caps, e := entry.ad.Discover(lease.ctx, entry.p)
		if e != nil {
			result.Providers = append(result.Providers, ProviderUpdate{entry.p.ID, "failed", e.Error()})
			result.Failed++
			continue
		}
		if !check {
			if _, e = a.cat.ReplaceProviderCapabilities(lease.ctx, entry.p, caps); e != nil {
				result.Providers = append(result.Providers, ProviderUpdate{entry.p.ID, "failed", e.Error()})
				result.Failed++
				continue
			}
		}
		state := "updated"
		if check {
			state = "checked"
		}
		result.Providers = append(result.Providers, ProviderUpdate{ID: entry.p.ID, State: state})
	}
	var roots []string
	if all {
		items, e := a.projections.ListWorkspaces(ctx)
		if e != nil {
			return result, e
		}
		for _, w := range items {
			roots = append(roots, w.Root)
		}
	} else {
		roots = []string{root}
	}
	for _, workspaceRoot := range roots {
		wr := WorkspaceUpdate{Root: workspaceRoot}
		status, e := a.projections.Status(ctx, workspaceRoot)
		if e != nil {
			wr.Failed++
			result.Failed++
			result.Workspaces = append(result.Workspaces, wr)
			continue
		}
		if check {
			wr.Unchanged = len(status.Skills)
			result.Workspaces = append(result.Workspaces, wr)
			continue
		}
		built, e := a.projections.UpdateBuiltin(ctx, workspaceRoot)
		if e != nil {
			wr.Failed++
			result.Failed++
		} else {
			wr.Updated += built.Updated
			wr.Unchanged += built.Unchanged
			wr.Repaired += built.Repaired
		}
		for _, item := range status.Skills {
			if item.SourceKind == "builtin" {
				continue
			}
			snapshot, snapshotErr := a.snapshotForManaged(ctx, item.SourceKind, item.SourceID)
			if errors.Is(snapshotErr, catalog.ErrNotFound) {
				if e = a.projections.Remove(ctx, workspaceRoot, item.LogicalID); e == nil {
					wr.Removed++
					continue
				}
			}
			if snapshotErr != nil {
				wr.Failed++
				result.Failed++
				continue
			}
			if snapshot.Digest == item.Digest {
				wr.Unchanged++
				continue
			}
			if _, e = a.projections.Register(ctx, workspaceRoot, item.TargetRoot, snapshot, item.SourceKind, item.SourceID); e != nil {
				wr.Failed++
				result.Failed++
			} else {
				wr.Updated++
			}
		}
		result.Workspaces = append(result.Workspaces, wr)
	}
	if result.Failed > 0 {
		return result, fmt.Errorf("update completed with %d failures", result.Failed)
	}
	return result, nil
}

func (a *App) snapshotForManaged(ctx context.Context, kind, source string) (mount.Snapshot, error) {
	switch kind {
	case "capability":
		c, err := a.cat.GetCapability(ctx, source)
		if err != nil {
			return mount.Snapshot{}, err
		}
		rendered, err := generator.Render(c, false)
		if err != nil {
			return mount.Snapshot{}, err
		}
		return mount.NewSnapshot(rendered.CapabilityID, rendered.Name, fmt.Sprintf("%d", rendered.Revision), rendered.Revision, rendered.Files)
	case "declarative":
		p, err := a.declarativeProvider(ctx, source)
		if err != nil {
			return mount.Snapshot{}, err
		}
		if p == nil {
			return mount.Snapshot{}, catalog.ErrNotFound
		}
		cfg, err := declarative.Parse([]byte(p.Config["source"]))
		if err != nil {
			return mount.Snapshot{}, err
		}
		rendered, err := declarative.RenderSkill(cfg)
		if err != nil {
			return mount.Snapshot{}, err
		}
		return mount.NewSnapshot(rendered.CapabilityID, rendered.Name, cfg.Digest, 1, rendered.Files)
	default:
		return mount.Snapshot{}, fmt.Errorf("unsupported managed skill kind %q", kind)
	}
}
