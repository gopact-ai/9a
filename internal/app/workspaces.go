package app

import (
	"context"
	"github.com/gopact-ai/9a/internal/projection"
	"github.com/gopact-ai/9a/internal/workspace"
)

func (a *App) AttachWorkspace(ctx context.Context, root string, policy workspace.BackendPolicy) (projection.Status, error) {
	return a.projections.Attach(ctx, root, policy)
}
func (a *App) WorkspaceStatus(ctx context.Context, root string) (projection.Status, error) {
	return a.projections.Status(ctx, root)
}
func (a *App) DetachWorkspace(ctx context.Context, root string) error {
	return a.projections.Detach(ctx, root)
}
