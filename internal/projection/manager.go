package projection

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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

type OptionalBackend interface {
	Available(context.Context) error
	Attach(context.Context, string, string, mount.Snapshot) (mount.Attachment, error)
	Update(context.Context, mount.Attachment, mount.Snapshot) (mount.Attachment, error)
	Inspect(context.Context, mount.Attachment, mount.Snapshot) (mount.Inspection, error)
	Detach(context.Context, mount.Attachment) error
	Close(context.Context) error
}

type Status struct {
	Workspace workspace.Workspace      `json:"workspace"`
	Skills    []workspace.ManagedSkill `json:"skills"`
}
type UpdateResult struct {
	Updated   int    `json:"updated"`
	Unchanged int    `json:"unchanged"`
	Repaired  int    `json:"repaired"`
	Failed    int    `json:"failed"`
	Status    Status `json:"status"`
}

type Manager struct {
	mu        sync.Mutex
	repo      *workspace.Repository
	directory *dir.Backend
	fuse      OptionalBackend
	builtin   mount.Snapshot
}

func New(db *sql.DB, builtin mount.Snapshot, fuse OptionalBackend) *Manager {
	return &Manager{repo: workspace.NewRepository(db), directory: dir.New(), fuse: fuse, builtin: builtin}
}
func workspaceID(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "ws-" + hex.EncodeToString(sum[:8])
}
func validPolicy(p workspace.BackendPolicy) bool {
	return p == workspace.PolicyAuto || p == workspace.PolicyFUSE || p == workspace.PolicyDirectory
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
		return m.ensureBuiltin(ctx, existing)
	} else if !errors.Is(err, workspace.ErrNotFound) {
		return Status{}, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return Status{}, err
	}
	now := time.Now().UTC()
	w := workspace.Workspace{ID: workspaceID(root), Root: root, SkillsRoot: filepath.Join(root, ".agents", "skills"), Policy: policy, Format: Format, CreatedAt: now, UpdatedAt: now}
	if policy != workspace.PolicyDirectory && m.fuse != nil {
		if err := m.fuse.Available(ctx); err == nil {
			w.Backend = workspace.BackendFUSE
			w.State = workspace.StateHealthy
		} else if policy == workspace.PolicyFUSE {
			return Status{}, err
		} else {
			w.Backend = workspace.BackendDirectory
			w.State = workspace.StateFallback
			w.FallbackReason = err.Error()
		}
	} else if policy == workspace.PolicyFUSE {
		return Status{}, fmt.Errorf("fuse backend is unavailable")
	} else {
		w.Backend = workspace.BackendDirectory
		w.State = workspace.StateHealthy
		if policy == workspace.PolicyAuto {
			w.State = workspace.StateFallback
			w.FallbackReason = "fuse backend is unavailable"
		}
	}
	a, err := m.attachBackend(ctx, w, m.builtin)
	if err != nil {
		if policy != workspace.PolicyAuto || w.Backend != workspace.BackendFUSE {
			return Status{}, err
		}
		w.Backend = workspace.BackendDirectory
		w.State = workspace.StateFallback
		w.FallbackReason = "fuse mount failed: " + err.Error()
		a, err = m.attachBackend(ctx, w, m.builtin)
		if err != nil {
			return Status{}, err
		}
	}
	if err = m.repo.PutWorkspace(ctx, w); err != nil {
		_ = m.detachBackend(ctx, w, a)
		return Status{}, err
	}
	record := recordFor(w, w.SkillsRoot, m.builtin, "builtin", "using-ninea")
	if err = m.repo.PutManagedSkill(ctx, record); err != nil {
		_ = m.detachBackend(ctx, w, a)
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
func (m *Manager) attachBackend(ctx context.Context, w workspace.Workspace, s mount.Snapshot) (mount.Attachment, error) {
	return m.attachBackendAt(ctx, w, w.SkillsRoot, s)
}
func (m *Manager) attachBackendAt(ctx context.Context, w workspace.Workspace, targetRoot string, s mount.Snapshot) (mount.Attachment, error) {
	if w.Backend == workspace.BackendFUSE {
		return m.fuse.Attach(ctx, targetRoot, w.ID, s)
	}
	return m.directory.Attach(ctx, targetRoot, w.ID, s)
}
func (m *Manager) detachBackend(ctx context.Context, w workspace.Workspace, a mount.Attachment) error {
	if w.Backend == workspace.BackendFUSE {
		return m.fuse.Detach(ctx, a)
	}
	return m.directory.Detach(ctx, a)
}
func (m *Manager) updateBackend(ctx context.Context, w workspace.Workspace, a mount.Attachment, s mount.Snapshot) (mount.Attachment, error) {
	if w.Backend == workspace.BackendFUSE {
		return m.fuse.Update(ctx, a, s)
	}
	return m.directory.Update(ctx, a, s)
}
func (m *Manager) inspectBackend(ctx context.Context, w workspace.Workspace, a mount.Attachment, s mount.Snapshot) (mount.Inspection, error) {
	if w.Backend == workspace.BackendFUSE {
		return m.fuse.Inspect(ctx, a, s)
	}
	return m.directory.Inspect(ctx, a, s)
}
func (m *Manager) ensureBuiltin(ctx context.Context, w workspace.Workspace) (Status, error) {
	items, err := m.repo.ListManagedSkills(ctx, w.ID)
	if err != nil {
		return Status{}, err
	}
	for i, item := range items {
		if item.LogicalID == m.builtin.LogicalID {
			inspect, e := m.inspectBackend(ctx, w, attachment(w, item), m.builtin)
			if e != nil {
				return Status{}, e
			}
			if item.Digest != m.builtin.Digest || inspect.State != mount.InspectionHealthy {
				if _, e = m.updateBackend(ctx, w, attachment(w, item), m.builtin); e != nil {
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
	a, e := m.attachBackend(ctx, w, m.builtin)
	if e != nil {
		return Status{}, e
	}
	_ = a
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

func (m *Manager) Register(ctx context.Context, workspaceRoot, targetRoot string, snapshot mount.Snapshot, kind, source string) (workspace.ManagedSkill, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.repo.GetWorkspaceByRoot(ctx, filepath.Clean(workspaceRoot))
	if err != nil {
		return workspace.ManagedSkill{}, err
	}
	if !filepath.IsAbs(targetRoot) {
		return workspace.ManagedSkill{}, fmt.Errorf("skill root must be absolute")
	}
	targetRoot = filepath.Clean(targetRoot)
	items, err := m.repo.ListManagedSkills(ctx, w.ID)
	if err != nil {
		return workspace.ManagedSkill{}, err
	}
	for _, item := range items {
		if item.LogicalID == snapshot.LogicalID {
			a := attachment(w, item)
			if item.TargetRoot != targetRoot || item.TargetName != snapshot.Name {
				nextAttachment, e := m.attachBackendAt(ctx, w, targetRoot, snapshot)
				if e != nil {
					return workspace.ManagedSkill{}, e
				}
				if e = m.detachBackend(ctx, w, a); e != nil {
					_ = m.detachBackend(ctx, w, nextAttachment)
					return workspace.ManagedSkill{}, e
				}
			} else if _, err = m.updateBackend(ctx, w, a, snapshot); err != nil {
				return workspace.ManagedSkill{}, err
			}
			next := recordFor(w, targetRoot, snapshot, kind, source)
			if err = m.repo.PutManagedSkill(ctx, next); err != nil {
				return workspace.ManagedSkill{}, err
			}
			return next, nil
		}
		if item.TargetRoot == targetRoot && item.TargetName == snapshot.Name {
			return workspace.ManagedSkill{}, fmt.Errorf("managed skill target conflicts with %s", item.LogicalID)
		}
	}
	a, err := m.attachBackendAt(ctx, w, targetRoot, snapshot)
	if err != nil {
		return workspace.ManagedSkill{}, err
	}
	next := recordFor(w, targetRoot, snapshot, kind, source)
	if err = m.repo.PutManagedSkill(ctx, next); err != nil {
		_ = m.detachBackend(ctx, w, a)
		return workspace.ManagedSkill{}, err
	}
	return next, nil
}
func (m *Manager) Remove(ctx context.Context, workspaceRoot, logicalID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.repo.GetWorkspaceByRoot(ctx, filepath.Clean(workspaceRoot))
	if err != nil {
		return err
	}
	items, err := m.repo.ListManagedSkills(ctx, w.ID)
	if err != nil {
		return err
	}
	for _, item := range items {
		if item.LogicalID == logicalID {
			if err = m.detachBackend(ctx, w, attachment(w, item)); err != nil {
				return err
			}
			return m.repo.DeleteManagedSkill(ctx, w.ID, logicalID)
		}
	}
	return nil
}
func (m *Manager) UpdateBuiltin(ctx context.Context, root string) (UpdateResult, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.repo.GetWorkspaceByRoot(ctx, filepath.Clean(root))
	if err != nil {
		return UpdateResult{}, err
	}
	items, err := m.repo.ListManagedSkills(ctx, w.ID)
	if err != nil {
		return UpdateResult{}, err
	}
	result := UpdateResult{}
	for i, item := range items {
		if item.LogicalID != m.builtin.LogicalID {
			continue
		}
		inspect, e := m.inspectBackend(ctx, w, attachment(w, item), m.builtin)
		if e != nil {
			return result, e
		}
		if item.Digest == m.builtin.Digest && inspect.State == mount.InspectionHealthy {
			result.Unchanged++
			continue
		}
		if _, e = m.updateBackend(ctx, w, attachment(w, item), m.builtin); e != nil {
			result.Failed++
			return result, e
		}
		if inspect.State == mount.InspectionTampered {
			result.Repaired++
		} else {
			result.Updated++
		}
		items[i] = recordFor(w, w.SkillsRoot, m.builtin, "builtin", "using-ninea")
		if e = m.repo.PutManagedSkill(ctx, items[i]); e != nil {
			return result, e
		}
	}
	result.Status = Status{Workspace: w, Skills: items}
	return result, nil
}

func (m *Manager) ListWorkspaces(ctx context.Context) ([]workspace.Workspace, error) {
	return m.repo.ListWorkspaces(ctx)
}

func (m *Manager) Inspect(ctx context.Context, w workspace.Workspace, item workspace.ManagedSkill, snapshot mount.Snapshot) (mount.Inspection, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.inspectBackend(ctx, w, attachment(w, item), snapshot)
}

func (m *Manager) RestoreSnapshot(ctx context.Context, w workspace.Workspace, item workspace.ManagedSkill, snapshot mount.Snapshot) (workspace.Workspace, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if w.Backend == workspace.BackendFUSE {
		_, err := m.attachBackendAt(ctx, w, item.TargetRoot, snapshot)
		if err == nil {
			return w, nil
		}
		if w.Policy != workspace.PolicyAuto {
			return w, err
		}
		w.Backend = workspace.BackendDirectory
		w.State = workspace.StateFallback
		w.FallbackReason = "fuse restore failed: " + err.Error()
		w.UpdatedAt = time.Now().UTC()
		if putErr := m.repo.PutWorkspace(ctx, w); putErr != nil {
			return w, errors.Join(err, putErr)
		}
		_, attachErr := m.directory.Attach(ctx, item.TargetRoot, w.ID, snapshot)
		return w, attachErr
	}
	inspection, err := m.directory.Inspect(ctx, attachment(w, item), snapshot)
	if err != nil {
		return w, err
	}
	if inspection.State == mount.InspectionHealthy {
		return w, nil
	}
	if inspection.State == mount.InspectionMissing {
		_, err = m.directory.Attach(ctx, item.TargetRoot, w.ID, snapshot)
		return w, err
	}
	_, err = m.directory.Update(ctx, attachment(w, item), snapshot)
	return w, err
}

func (m *Manager) RemoveBySource(ctx context.Context, kind, source string) error {
	workspaces, err := m.repo.ListWorkspaces(ctx)
	if err != nil {
		return err
	}
	for _, w := range workspaces {
		items, e := m.repo.ListManagedSkills(ctx, w.ID)
		if e != nil {
			return e
		}
		for _, item := range items {
			if item.SourceKind == kind && item.SourceID == source {
				if e = m.Remove(ctx, w.Root, item.LogicalID); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func (m *Manager) Detach(ctx context.Context, root string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	w, err := m.repo.GetWorkspaceByRoot(ctx, filepath.Clean(root))
	if errors.Is(err, workspace.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	items, err := m.repo.ListManagedSkills(ctx, w.ID)
	if err != nil {
		return err
	}
	for i := len(items) - 1; i >= 0; i-- {
		if err := m.detachBackend(ctx, w, attachment(w, items[i])); err != nil {
			return err
		}
	}
	if err := m.repo.DeleteWorkspace(ctx, w.ID); err != nil {
		return err
	}
	removeEmpty(w.SkillsRoot)
	removeEmpty(filepath.Dir(w.SkillsRoot))
	return nil
}
func removeEmpty(path string) { _ = os.Remove(path) }
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fuse != nil {
		return m.fuse.Close(ctx)
	}
	return nil
}
