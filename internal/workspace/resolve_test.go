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

	t.Run("managed skills directory resolves to owning workspace", func(t *testing.T) {
		root := t.TempDir()
		root, _ = filepath.EvalSymlinks(root)
		path := filepath.Join(root, ".agents", "skills", "using-ninea", "references")
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := Resolve("", path)
		if err != nil || got != root {
			t.Fatalf("Resolve(%q)=%q err=%v want %q", path, got, err, root)
		}
	})

	t.Run("managed skills directory wins over enclosing git worktree", func(t *testing.T) {
		repo := t.TempDir()
		if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		owner := filepath.Join(repo, "app")
		cwd := filepath.Join(owner, ".agents", "skills", "using-ninea", "references")
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		owner, _ = filepath.EvalSymlinks(owner)
		got, err := Resolve("", cwd)
		if err != nil || got != owner {
			t.Fatalf("Resolve()=%q err=%v want %q", got, err, owner)
		}
	})

	t.Run("unrelated skill repository resolves to itself", func(t *testing.T) {
		owner := t.TempDir()
		skill := filepath.Join(owner, ".agents", "skills", "custom")
		if err := os.MkdirAll(skill, 0o755); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command("git", "-C", skill, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		cwd := filepath.Join(skill, "references")
		if err := os.Mkdir(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		skill, _ = filepath.EvalSymlinks(skill)
		got, err := Resolve("", cwd)
		if err != nil || got != skill {
			t.Fatalf("Resolve()=%q err=%v want %q", got, err, skill)
		}
	})

	t.Run("symlinked unrelated skill resolves to its repository", func(t *testing.T) {
		owner := t.TempDir()
		source := t.TempDir()
		if out, err := exec.Command("git", "-C", source, "init", "-q").CombinedOutput(); err != nil {
			t.Fatalf("git init: %v: %s", err, out)
		}
		sourceSkill := filepath.Join(source, "custom")
		cwdSuffix := filepath.Join("references", "api")
		if err := os.MkdirAll(filepath.Join(sourceSkill, cwdSuffix), 0o755); err != nil {
			t.Fatal(err)
		}
		skillsRoot := filepath.Join(owner, ".agents", "skills")
		if err := os.MkdirAll(skillsRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sourceSkill, filepath.Join(skillsRoot, "custom")); err != nil {
			t.Fatal(err)
		}
		source, _ = filepath.EvalSymlinks(source)
		got, err := Resolve("", filepath.Join(skillsRoot, "custom", cwdSuffix))
		if err != nil || got != source {
			t.Fatalf("Resolve()=%q err=%v want %q", got, err, source)
		}
	})

	t.Run("missing path fails", func(t *testing.T) {
		if _, err := Resolve(filepath.Join(t.TempDir(), "missing"), ""); err == nil {
			t.Fatal("missing path accepted")
		}
	})
}
