package app

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func canonicalWorkspaceRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("workspace root must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("resolve workspace root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("stat workspace root: %w", err)
	}
	if !info.IsDir() {
		return "", errors.New("workspace root must be a directory")
	}
	return filepath.Clean(canonical), nil
}
