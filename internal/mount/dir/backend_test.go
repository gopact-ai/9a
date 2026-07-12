package dir

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/9a/internal/mount"
)

func TestPublishAndRemoveOwnedSkill(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	b := New()
	s := mount.Skill{Name: "ninea-demo", CapabilityID: "mcp/demo/x", Revision: 1, Files: []mount.File{{Path: "SKILL.md", Mode: 0644, Data: []byte("hello")}, {Path: "scripts/invoke", Mode: 0755, Data: []byte("#!/bin/sh\n")}}}
	if err := b.Publish(context.Background(), root, s); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, s.Name, "SKILL.md"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("read=%q err=%v", data, err)
	}
	info, err := os.Stat(filepath.Join(root, s.Name, "scripts/invoke"))
	if err != nil || info.Mode()&0111 == 0 {
		t.Fatal("invoke not executable")
	}
	if err := b.Remove(context.Background(), root, s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, s.Name)); !os.IsNotExist(err) {
		t.Fatal("owned skill remains")
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
	s := mount.Skill{Name: "ninea-demo", CapabilityID: "mcp/demo/x", Revision: 1, Files: []mount.File{{Path: "SKILL.md", Mode: 0644, Data: []byte("generated")}}}
	if err := New().Publish(context.Background(), root, s); err == nil {
		t.Fatal("user directory overwritten")
	}
	if data, _ := os.ReadFile(filepath.Join(target, "mine")); string(data) != "user" {
		t.Fatal("user file changed")
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
	s := mount.Skill{Name: "ninea-demo", CapabilityID: "mcp/demo/x", Revision: 1, Files: []mount.File{{Path: "SKILL.md", Mode: 0644, Data: []byte("generated")}}}
	if err := New().Publish(context.Background(), root, s); err == nil {
		t.Fatal("regular file overwritten")
	}
	if data, _ := os.ReadFile(target); string(data) != "user" {
		t.Fatal("target changed")
	}
	if data, _ := os.ReadFile(filepath.Join(backup, "mine")); string(data) != "backup" {
		t.Fatal("unrelated backup changed")
	}
}
