// Package projection manages the projection of Skills into a workspace's
// skills directory. It attaches workspaces, keeps the built-in Skill present
// and up to date via the directory mount backend, and reports workspace and
// managed-skill status.
package projection

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gopact-ai/9a/internal/mount"
	"github.com/gopact-ai/9a/internal/mount/dir"
	"github.com/gopact-ai/9a/internal/workspace"
)

const Format = 1

type Status struct {
	Workspace workspace.Workspace      `json:"workspace"`
	Skills    []workspace.ManagedSkill `json:"skills"`
}

type Manager struct {
	mu        sync.Mutex
	repo      *workspace.Repository
	directory *dir.Backend
	builtin   mount.Snapshot
}

func New(db *sql.DB, builtin mount.Snapshot) *Manager {
	return &Manager{repo: workspace.NewRepository(db), directory: dir.New(), builtin: builtin}
}
func validPolicy(p workspace.BackendPolicy) bool {
	return p == workspace.PolicyAuto || p == workspace.PolicyDirectory
}

func (m *Manager) Attach(ctx context.Context, root string, policy workspace.BackendPolicy) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !filepath.IsAbs(root) {
		return Status{}, fmt.Errorf("workspace root must be absolute")
	}
	if !validPolicy(policy) {
		return Status{}, fmt.Errorf("invalid backend policy %q", policy)
	}
	root = filepath.Clean(root)
	if existing, err := m.repo.GetWorkspaceByRoot(ctx, root); err == nil {
		if policy != workspace.PolicyAuto {
			existing.Policy = policy
		}
		existing.Backend = workspace.BackendDirectory
		existing.State = workspace.StateHealthy
		status, err := m.ensureBuiltin(ctx, existing)
		if err != nil {
			return Status{}, err
		}
		existing.UpdatedAt = time.Now().UTC()
		if err = m.repo.PutWorkspace(ctx, existing); err != nil {
			return Status{}, err
		}
		status.Workspace = existing
		return status, nil
	} else if !errors.Is(err, workspace.ErrNotFound) {
		return Status{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Status{}, err
	}
	now := time.Now().UTC()
	w := workspace.Workspace{ID: workspace.StableID(root), Root: root, SkillsRoot: filepath.Join(root, ".agents", "skills"), Policy: policy, Backend: workspace.BackendDirectory, State: workspace.StateHealthy, Format: Format, CreatedAt: now, UpdatedAt: now}
	a, err := m.directory.Attach(ctx, w.SkillsRoot, w.ID, m.builtin)
	if err != nil {
		return Status{}, err
	}
	if err = m.repo.PutWorkspace(ctx, w); err != nil {
		_ = m.directory.Detach(ctx, a)
		return Status{}, err
	}
	record := recordFor(w, w.SkillsRoot, m.builtin, "builtin", "using-ninea")
	if err = m.repo.PutManagedSkill(ctx, record); err != nil {
		_ = m.directory.Detach(ctx, a)
		_ = m.repo.DeleteWorkspace(ctx, w.ID)
		return Status{}, err
	}
	return Status{Workspace: w, Skills: []workspace.ManagedSkill{record}}, nil
}
func recordFor(w workspace.Workspace, targetRoot string, s mount.Snapshot, kind, source string) workspace.ManagedSkill {
	return workspace.ManagedSkill{WorkspaceID: w.ID, LogicalID: s.LogicalID, TargetRoot: targetRoot, TargetName: s.Name, SourceKind: kind, SourceID: source, CatalogRevision: s.CatalogRevision, SkillVersion: s.Version, Digest: s.Digest, MountState: "attached", UpdatedAt: time.Now().UTC()}
}
func attachment(w workspace.Workspace, s workspace.ManagedSkill) mount.Attachment {
	return mount.Attachment{WorkspaceID: w.ID, LogicalID: s.LogicalID, Target: filepath.Join(s.TargetRoot, s.TargetName)}
}
func (m *Manager) ensureBuiltin(ctx context.Context, w workspace.Workspace) (Status, error) {
	items, err := m.repo.ListManagedSkills(ctx, w.ID)
	if err != nil {
		return Status{}, err
	}
	for i, item := range items {
		if item.LogicalID == m.builtin.LogicalID {
			inspect, e := m.directory.Inspect(ctx, attachment(w, item), m.builtin)
			if e != nil {
				return Status{}, e
			}
			if item.Digest != m.builtin.Digest || inspect.State != mount.InspectionHealthy {
				if _, e = m.directory.Update(ctx, attachment(w, item), m.builtin); e != nil {
					return Status{}, e
				}
				items[i] = recordFor(w, w.SkillsRoot, m.builtin, "builtin", "using-ninea")
				if e = m.repo.PutManagedSkill(ctx, items[i]); e != nil {
					return Status{}, e
				}
			}
			return Status{Workspace: w, Skills: items}, nil
		}
	}
	_, e := m.directory.Attach(ctx, w.SkillsRoot, w.ID, m.builtin)
	if e != nil {
		return Status{}, e
	}
	item := recordFor(w, w.SkillsRoot, m.builtin, "builtin", "using-ninea")
	if e = m.repo.PutManagedSkill(ctx, item); e != nil {
		return Status{}, e
	}
	items = append(items, item)
	return Status{Workspace: w, Skills: items}, nil
}
func (m *Manager) Status(ctx context.Context, root string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.repo.GetWorkspaceByRoot(ctx, filepath.Clean(root))
	if errors.Is(err, workspace.ErrNotFound) {
		return Status{Workspace: workspace.Workspace{Root: filepath.Clean(root), State: workspace.StateDetached}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	items, err := m.repo.ListManagedSkills(ctx, w.ID)
	return Status{Workspace: w, Skills: items}, err
}

func (m *Manager) Inspect(ctx context.Context, w workspace.Workspace, item workspace.ManagedSkill, snapshot mount.Snapshot) (mount.Inspection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.directory.Inspect(ctx, attachment(w, item), snapshot)
}
func (m *Manager) Close(context.Context) error {
	return nil
}
