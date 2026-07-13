package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveWorkspace(t *testing.T) {
	t.Run("explicit path wins and resolves symlinks", func(t *testing.T) {
		root := t.TempDir()
		target := filepath.Join(root, "target")
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(root, "link")
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
		target, _ = filepath.EvalSymlinks(target)
		got, err := Resolve(link, root)
		if err != nil || got != target {
			t.Fatalf("Resolve()=%q err=%v want %q", got, err, target)
		}
	})

	t.Run("git child resolves to worktree root", func(t *testing.T) {
		root := t.TempDir()
		if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		child := filepath.Join(root, "a", "b")
		if err := os.MkdirAll(child, 0o755); err != nil {
			t.Fatal(err)
		}
		root, _ = filepath.EvalSymlinks(root)
		got, err := Resolve("", child)
		if err != nil || got != root {
			t.Fatalf("Resolve()=%q err=%v want %q", got, err, root)
		}
	})

	t.Run("non git directory resolves to itself", func(t *testing.T) {
		root := t.TempDir()
		root, _ = filepath.EvalSymlinks(root)
		got, err := Resolve("", root)
		if err != nil || got != root {
			t.Fatalf("Resolve()=%q err=%v want %q", got, err, root)
		}
	})

	t.Run("missing path fails", func(t *testing.T) {
		if _, err := Resolve(filepath.Join(t.TempDir(), "missing"), ""); err == nil {
			t.Fatal("missing path accepted")
		}
	})
}
