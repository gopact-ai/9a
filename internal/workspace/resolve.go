package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Resolve returns the canonical workspace directory. An explicit path wins;
// otherwise the enclosing Git worktree is used when one exists.
func Resolve(explicit, cwd string) (string, error) {
	path := explicit
	if path == "" {
		path = cwd
	}
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", canonical)
	}
	if explicit != "" {
		return filepath.Clean(canonical), nil
	}
	command := exec.Command("git", "-C", canonical, "rev-parse", "--show-toplevel")
	output, gitErr := command.Output()
	if gitErr != nil {
		return filepath.Clean(canonical), nil
	}
	root := strings.TrimSpace(string(output))
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve git workspace: %w", err)
	}
	return filepath.Clean(root), nil
}
