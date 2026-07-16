package workspace

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Resolve returns the canonical workspace directory. An explicit path wins;
// otherwise the NineA gateway Skill selects its owner before Git discovery.
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
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("stat workspace: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("workspace is not a directory: %s", abs)
	}
	if explicit == "" {
		for path := filepath.Clean(abs); ; path = filepath.Dir(path) {
			parent := filepath.Dir(path)
			agents := filepath.Dir(parent)
			if filepath.Base(path) == "using-ninea" && filepath.Base(parent) == "skills" && filepath.Base(agents) == ".agents" {
				owner, resolveErr := filepath.EvalSymlinks(filepath.Dir(agents))
				if resolveErr != nil {
					return "", fmt.Errorf("resolve workspace: %w", resolveErr)
				}
				return filepath.Clean(owner), nil
			}
			if parent == path {
				break
			}
		}
	}
	canonical, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve workspace: %w", err)
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
