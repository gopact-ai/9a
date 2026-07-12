package app

import (
	"context"
	"github.com/gopact-ai/9a/internal/buildinfo"
	"github.com/gopact-ai/9a/internal/builtin"
)

func (a *App) restoreManagedViews(ctx context.Context) error {
	workspaces, err := a.projections.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	built, err := builtin.UsingNineA(buildinfo.Version)
	if err != nil {
		return err
	}
	for _, w := range workspaces {
		status, e := a.projections.Status(ctx, w.Root)
		if e != nil {
			return e
		}
		for _, item := range status.Skills {
			snapshot := built
			if item.SourceKind != "builtin" {
				snapshot, e = a.snapshotForManaged(ctx, item.SourceKind, item.SourceID)
				if e != nil {
					return e
				}
			}
			if e = a.projections.RestoreSnapshot(ctx, w, item, snapshot); e != nil {
				return e
			}
		}
	}
	return nil
}
