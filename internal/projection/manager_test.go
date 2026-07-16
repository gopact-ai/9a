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

func TestAttachAndStatusGatewayDirectory(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	snapshot, err := builtin.UsingNineA("v1")
	if err != nil {
		t.Fatal(err)
	}
	m := New(db, snapshot)
	root := filepath.Join(t.TempDir(), "workspace")
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
	if _, err := m.Attach(ctx, root, workspace.BackendPolicy("fuse")); err == nil {
		t.Fatal("removed fuse policy accepted")
	}
	if status, err := m.Attach(ctx, root, workspace.PolicyAuto); err != nil {
		t.Fatal(err)
	} else if status.Workspace.Backend != workspace.BackendDirectory || status.Workspace.State != workspace.StateHealthy {
		t.Fatalf("status=%#v", status)
	}
	status, err := m.Status(ctx, root)
	if err != nil || len(status.Skills) != 1 || status.Skills[0].LogicalID != "builtin/using-ninea" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestAttachNeverOverwritesUserSkill(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	snapshot, _ := builtin.UsingNineA("v1")
	m := New(db, snapshot)
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
