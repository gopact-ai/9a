//go:build linux || darwin

package fusemount

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/9a/internal/mount"
)

func TestRequiredFUSERuntime(t *testing.T) {
	if os.Getenv("NINEA_REQUIRE_FUSE") != "1" {
		t.Skip("set NINEA_REQUIRE_FUSE=1 on a FUSE-enabled runner")
	}
	backend := New()
	if err := backend.Available(context.Background()); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := mount.NewSnapshot("test/readonly", "readonly", "v1", 1, []mount.File{{Path: "SKILL.md", Mode: 0o444, Data: []byte("hello")}})
	root := t.TempDir()
	attachment, err := backend.Attach(context.Background(), root, "workspace", snapshot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Detach(context.Background(), attachment) })
	data, err := os.ReadFile(filepath.Join(attachment.Target, "SKILL.md"))
	if err != nil || string(data) != "hello" {
		t.Fatalf("read=%q err=%v", data, err)
	}
	if err = os.WriteFile(filepath.Join(attachment.Target, "SKILL.md"), []byte("changed"), 0o644); err == nil {
		t.Fatal("FUSE projection accepted write")
	}
	if err = backend.Detach(context.Background(), attachment); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(attachment.Target); !os.IsNotExist(err) {
		t.Fatalf("mountpoint remains: %v", err)
	}
}
