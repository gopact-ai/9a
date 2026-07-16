package adapter

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

var ErrInvalid = errors.New("invalid executable")

func ValidateExecutable(executable string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("%w: path must be absolute", ErrInvalid)
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("%w: path cannot be resolved", ErrInvalid)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: path is unavailable", ErrInvalid)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: path is not a regular file", ErrInvalid)
	}
	if info.Mode().Perm()&0o111 == 0 || unix.Access(canonical, unix.X_OK) != nil {
		return "", fmt.Errorf("%w: path is not executable", ErrInvalid)
	}
	return filepath.Clean(canonical), nil
}
