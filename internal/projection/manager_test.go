package projection

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/builtin"
	"github.com/gopact-ai/9a/internal/mount"
	"github.com/gopact-ai/9a/internal/store"
	"github.com/gopact-ai/9a/internal/workspace"
)

func TestAttachStatusUpdateDetachDirectoryFallback(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot, err := builtin.UsingNineA("v1")
	if err != nil {
		t.Fatal(err)
	}
	m := New(db, snapshot, nil)
	root := filepath.Join(t.TempDir(), "workspace")
	if status, err := m.Attach(ctx, root, workspace.PolicyAuto); err != nil {
		t.Fatal(err)
	} else if status.Workspace.Backend != workspace.BackendDirectory || status.Workspace.State != workspace.StateFallback {
		t.Fatalf("status=%#v", status)
	}
	status, err := m.Status(ctx, root)
	if err != nil || len(status.Skills) != 1 || status.Skills[0].LogicalID != "builtin/using-ninea" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if _, err := m.UpdateBuiltin(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach(ctx, root); err != nil {
		t.Fatal(err)
	}
	if err := m.Detach(ctx, root); err != nil {
		t.Fatalf("idempotent detach: %v", err)
	}
}

type unavailableFUSE struct{}

func (*unavailableFUSE) Available(context.Context) error { return errors.New("unavailable") }
func (*unavailableFUSE) Attach(context.Context, string, string, mount.Snapshot) (mount.Attachment, error) {
	return mount.Attachment{}, errors.New("mount failed")
}
func (*unavailableFUSE) Update(context.Context, mount.Attachment, mount.Snapshot) (mount.Attachment, error) {
	return mount.Attachment{}, errors.New("mount failed")
}
func (*unavailableFUSE) Inspect(context.Context, mount.Attachment, mount.Snapshot) (mount.Inspection, error) {
	return mount.Inspection{}, errors.New("mount failed")
}
func (*unavailableFUSE) Detach(context.Context, mount.Attachment) error { return nil }
func (*unavailableFUSE) Close(context.Context) error                    { return nil }

func TestRestoreAutoWorkspaceFallsBackWhenFUSEDisappears(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot, _ := builtin.UsingNineA("v1")
	m := New(db, snapshot, &unavailableFUSE{})
	root := t.TempDir()
	w := workspace.Workspace{ID: "ws", Root: root, SkillsRoot: filepath.Join(root, ".agents", "skills"), Policy: workspace.PolicyAuto, Backend: workspace.BackendFUSE, State: workspace.StateHealthy, Format: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err = m.repo.PutWorkspace(ctx, w); err != nil {
		t.Fatal(err)
	}
	item := recordFor(w, w.SkillsRoot, snapshot, "builtin", "using-ninea")
	if err = m.repo.PutManagedSkill(ctx, item); err != nil {
		t.Fatal(err)
	}
	restored, err := m.RestoreSnapshot(ctx, w, item, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Backend != workspace.BackendDirectory || restored.State != workspace.StateFallback {
		t.Fatalf("restored=%#v", restored)
	}
	if _, err = os.Stat(filepath.Join(w.SkillsRoot, "using-ninea", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err = m.Detach(ctx, root); err != nil {
		t.Fatal(err)
	}
}

func TestAttachNeverOverwritesUserSkill(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	snapshot, _ := builtin.UsingNineA("v1")
	m := New(db, snapshot, nil)
	root := t.TempDir()
	target := filepath.Join(root, ".agents", "skills", "using-ninea")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "mine"), []byte("user"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Attach(ctx, root, workspace.PolicyDirectory); err == nil {
		t.Fatal("user skill overwritten")
	}
}
