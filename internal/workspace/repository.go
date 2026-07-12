package workspace

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

var ErrNotFound = errors.New("workspace not found")

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

func (r *Repository) PutWorkspace(ctx context.Context, w Workspace) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO workspaces(id,root,skills_root,policy,backend,state,fallback_reason,format,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET root=excluded.root,skills_root=excluded.skills_root,policy=excluded.policy,backend=excluded.backend,state=excluded.state,fallback_reason=excluded.fallback_reason,format=excluded.format,updated_at=excluded.updated_at`, w.ID, w.Root, w.SkillsRoot, w.Policy, w.Backend, w.State, w.FallbackReason, w.Format, w.CreatedAt.Format(time.RFC3339Nano), w.UpdatedAt.Format(time.RFC3339Nano))
	return err
}
func scanWorkspace(row interface{ Scan(...any) error }) (Workspace, error) {
	var w Workspace
	var created, updated string
	err := row.Scan(&w.ID, &w.Root, &w.SkillsRoot, &w.Policy, &w.Backend, &w.State, &w.FallbackReason, &w.Format, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return w, ErrNotFound
	}
	if err != nil {
		return w, err
	}
	w.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return w, err
	}
	w.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return w, err
}
func (r *Repository) GetWorkspaceByRoot(ctx context.Context, root string) (Workspace, error) {
	return scanWorkspace(r.db.QueryRowContext(ctx, `SELECT id,root,skills_root,policy,backend,state,fallback_reason,format,created_at,updated_at FROM workspaces WHERE root=?`, root))
}
func (r *Repository) GetWorkspace(ctx context.Context, id string) (Workspace, error) {
	return scanWorkspace(r.db.QueryRowContext(ctx, `SELECT id,root,skills_root,policy,backend,state,fallback_reason,format,created_at,updated_at FROM workspaces WHERE id=?`, id))
}
func (r *Repository) ListWorkspaces(ctx context.Context) ([]Workspace, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,root,skills_root,policy,backend,state,fallback_reason,format,created_at,updated_at FROM workspaces ORDER BY root`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		w, e := scanWorkspace(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func (r *Repository) DeleteWorkspace(ctx context.Context, id string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM workspaces WHERE id=?`, id)
	return err
}
func (r *Repository) PutManagedSkill(ctx context.Context, s ManagedSkill) error {
	_, err := r.db.ExecContext(ctx, `INSERT INTO managed_skills(workspace_id,logical_id,target_name,source_kind,source_id,catalog_revision,skill_version,digest,mount_state,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(workspace_id,logical_id) DO UPDATE SET target_name=excluded.target_name,source_kind=excluded.source_kind,source_id=excluded.source_id,catalog_revision=excluded.catalog_revision,skill_version=excluded.skill_version,digest=excluded.digest,mount_state=excluded.mount_state,updated_at=excluded.updated_at`, s.WorkspaceID, s.LogicalID, s.TargetName, s.SourceKind, s.SourceID, s.CatalogRevision, s.SkillVersion, s.Digest, s.MountState, s.UpdatedAt.Format(time.RFC3339Nano))
	return err
}
func (r *Repository) ListManagedSkills(ctx context.Context, workspaceID string) ([]ManagedSkill, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT workspace_id,logical_id,target_name,source_kind,source_id,catalog_revision,skill_version,digest,mount_state,updated_at FROM managed_skills WHERE workspace_id=? ORDER BY target_name`, workspaceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ManagedSkill
	for rows.Next() {
		var s ManagedSkill
		var updated string
		if err := rows.Scan(&s.WorkspaceID, &s.LogicalID, &s.TargetName, &s.SourceKind, &s.SourceID, &s.CatalogRevision, &s.SkillVersion, &s.Digest, &s.MountState, &updated); err != nil {
			return nil, err
		}
		s.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (r *Repository) DeleteManagedSkill(ctx context.Context, workspaceID, logicalID string) error {
	_, err := r.db.ExecContext(ctx, `DELETE FROM managed_skills WHERE workspace_id=? AND logical_id=?`, workspaceID, logicalID)
	return err
}
