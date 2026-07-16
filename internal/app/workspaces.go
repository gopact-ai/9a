package app

import (
	"context"

	"github.com/gopact-ai/9a/internal/buildinfo"
	"github.com/gopact-ai/9a/internal/builtin"
	"github.com/gopact-ai/9a/internal/mount"
	"github.com/gopact-ai/9a/internal/projection"
	"github.com/gopact-ai/9a/internal/workspace"
)

func (a *App) workspaceStatus(ctx context.Context, root string) (projection.Status, error) {
	canonical, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return projection.Status{}, err
	}
	root = canonical
	status, err := a.projections.Status(ctx, root)
	if err != nil || status.Workspace.State == workspace.StateDetached {
		return status, err
	}
	built, err := builtin.UsingNineA(buildinfo.Version)
	if err != nil {
		return status, err
	}
	for _, item := range status.Skills {
		if item.SourceKind != "builtin" {
			continue
		}
		inspection, inspectErr := a.projections.Inspect(ctx, status.Workspace, item, built)
		if inspectErr != nil {
			status.Workspace.State = workspace.StateDegraded
			continue
		}
		if inspection.State == mount.InspectionTampered || inspection.State == mount.InspectionMissing {
			status.Workspace.State = workspace.StateTampered
		}
	}
	return status, nil
}
