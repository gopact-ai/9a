package app

import (
	"context"
	"github.com/gopact-ai/9a/internal/buildinfo"
	"github.com/gopact-ai/9a/internal/builtin"
	"github.com/gopact-ai/9a/internal/mount"
	"github.com/gopact-ai/9a/internal/projection"
	"github.com/gopact-ai/9a/internal/workspace"
)

func (a *App) AttachWorkspace(ctx context.Context, root string, policy workspace.BackendPolicy) (projection.Status, error) {
	return a.projections.Attach(ctx, root, policy)
}
func (a *App) WorkspaceStatus(ctx context.Context, root string) (projection.Status, error) {
	status, err := a.projections.Status(ctx, root)
	if err != nil || status.Workspace.State == workspace.StateDetached {
		return status, err
	}
	built, err := builtin.UsingNineA(buildinfo.Version)
	if err != nil {
		return status, err
	}
	for i, item := range status.Skills {
		snapshot := built
		if item.SourceKind != "builtin" {
			snapshot, err = a.snapshotForManaged(ctx, item.SourceKind, item.SourceID)
			if err != nil {
				status.Skills[i].MountState = "unavailable"
				status.Workspace.State = workspace.StateDegraded
				continue
			}
		}
		inspection, inspectErr := a.projections.Inspect(ctx, status.Workspace, item, snapshot)
		if inspectErr != nil {
			status.Skills[i].MountState = "unavailable"
			status.Workspace.State = workspace.StateDegraded
			continue
		}
		status.Skills[i].MountState = string(inspection.State)
		if inspection.State == mount.InspectionTampered || inspection.State == mount.InspectionMissing {
			status.Workspace.State = workspace.StateTampered
		}
	}
	return status, nil
}
func (a *App) DetachWorkspace(ctx context.Context, root string) error {
	a.mutation.Lock()
	defer a.mutation.Unlock()
	status, err := a.projections.Status(ctx, root)
	if err != nil {
		return err
	}
	if status.Workspace.State != workspace.StateDetached {
		if err = a.removeLocalSkills(ctx, status.Workspace); err != nil {
			return err
		}
	}
	return a.projections.Detach(ctx, root)
}
