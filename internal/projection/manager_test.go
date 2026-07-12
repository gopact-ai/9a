package projection

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/9a/internal/builtin"
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
