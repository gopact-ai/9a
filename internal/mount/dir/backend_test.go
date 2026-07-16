package dir

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/9a/internal/mount"
)

func TestManagedSnapshotIsReadOnlyAndDetectsTampering(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	snapshot, err := mount.NewSnapshot("builtin/using-ninea", "using-ninea", "v1", 1, []mount.File{{Path: "SKILL.md", Mode: 0o644, Data: []byte("hello")}, {Path: "scripts/invoke", Mode: 0o755, Data: []byte("#!/bin/sh\n")}})
	if err != nil {
		t.Fatal(err)
	}
	b := New()
	attachment, err := b.Attach(context.Background(), root, "workspace-1", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{"SKILL.md": 0o444, "scripts/invoke": 0o555, ".ninea-owned.json": 0o400} {
		info, err := os.Stat(filepath.Join(attachment.Target, path))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode=%o want=%o", path, info.Mode().Perm(), want)
		}
	}
	inspection, err := b.Inspect(context.Background(), attachment, snapshot)
	if err != nil || inspection.State != mount.InspectionHealthy {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	if err := os.Chmod(filepath.Join(attachment.Target, "SKILL.md"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(attachment.Target, "SKILL.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	inspection, err = b.Inspect(context.Background(), attachment, snapshot)
	if err != nil || inspection.State != mount.InspectionTampered {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	if _, err := b.Update(context.Background(), attachment, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(attachment.Target, "references"), 0o755); err == nil {
		_ = os.WriteFile(filepath.Join(attachment.Target, "references", "extra"), []byte("x"), 0o644)
	} else {
		if err := os.Chmod(attachment.Target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(attachment.Target, "extra"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	inspection, err = b.Inspect(context.Background(), attachment, snapshot)
	if err != nil || inspection.State != mount.InspectionTampered {
		t.Fatalf("extra file inspection=%#v err=%v", inspection, err)
	}
	if _, err := b.Update(context.Background(), attachment, snapshot); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(attachment.Target, "SKILL.md"))
	if string(data) != "hello" {
		t.Fatalf("repair=%q", data)
	}
	if err := b.Detach(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(attachment.Target); !os.IsNotExist(err) {
		t.Fatalf("target remains: %v", err)
	}
}

func TestPublishNeverOverwritesUserDirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "ninea-demo")
	if err := os.Mkdir(target, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "mine"), []byte("user"), 0644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := mount.NewSnapshot("builtin/using-ninea", "ninea-demo", "v1", 1, []mount.File{{Path: "SKILL.md", Mode: 0o644, Data: []byte("generated")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New().Attach(context.Background(), root, "workspace-1", snapshot); err == nil {
		t.Fatal("user directory overwritten")
	}
	if data, _ := os.ReadFile(filepath.Join(target, "mine")); string(data) != "user" {
		t.Fatal("user file changed")
	}
}

func TestAttachRejectsSymlinkedManagedRoot(t *testing.T) {
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, ".agents")); err != nil {
		t.Fatal(err)
	}
	snapshot, err := mount.NewSnapshot("builtin/using-ninea", "using-ninea", "v1", 1, []mount.File{{Path: "SKILL.md", Mode: 0o644, Data: []byte("hello")}})
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(workspace, ".agents", "skills")
	if _, err := New().Attach(context.Background(), root, "workspace-1", snapshot); !errors.Is(err, ErrConflict) {
		t.Fatalf("Attach error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "skills", "using-ninea")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside workspace was modified: %v", err)
	}
}

func TestPublishNeverOverwritesRegularFileOrFixedBackupName(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	target := filepath.Join(root, "ninea-demo")
	if err := os.WriteFile(target, []byte("user"), 0644); err != nil {
		t.Fatal(err)
	}
	backup := target + ".ninea-old"
	if err := os.Mkdir(backup, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backup, "mine"), []byte("backup"), 0644); err != nil {
		t.Fatal(err)
	}
	snapshot, err := mount.NewSnapshot("builtin/using-ninea", "ninea-demo", "v1", 1, []mount.File{{Path: "SKILL.md", Mode: 0o644, Data: []byte("generated")}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := New().Attach(context.Background(), root, "workspace-1", snapshot); err == nil {
		t.Fatal("regular file overwritten")
	}
	if data, _ := os.ReadFile(target); string(data) != "user" {
		t.Fatal("target changed")
	}
	if data, _ := os.ReadFile(filepath.Join(backup, "mine")); string(data) != "backup" {
		t.Fatal("unrelated backup changed")
	}
}
