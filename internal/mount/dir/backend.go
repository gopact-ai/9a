package dir

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/9a/internal/mount"
)

const marker = ".ninea-owned.json"

var ErrConflict = errors.New("projection conflicts with unowned directory")

type Backend struct{}

func New() *Backend { return &Backend{} }

func (b *Backend) Publish(ctx context.Context, root string, s mount.Skill) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := filepath.Join(root, s.Name)
	if info, err := os.Lstat(target); err == nil {
		if !info.IsDir() {
			return ErrConflict
		}
		owned, readErr := readMarker(target)
		if readErr != nil || owned.CapabilityID != s.CapabilityID {
			return ErrConflict
		}
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(root, 0755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".ninea-stage-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	for _, f := range s.Files {
		clean := filepath.Clean(f.Path)
		if clean == "." || filepath.IsAbs(clean) || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
			return fmt.Errorf("unsafe projected path %q", f.Path)
		}
		path := filepath.Join(stage, clean)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(path, f.Data, f.Mode); err != nil {
			return err
		}
	}
	data, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(stage, marker), data, 0600); err != nil {
		return err
	}
	if _, err := os.Lstat(target); err == nil {
		backup, err := os.MkdirTemp(root, ".ninea-backup-")
		if err != nil {
			return err
		}
		if err := os.Remove(backup); err != nil {
			return err
		}
		if err := os.Rename(target, backup); err != nil {
			return err
		}
		if err := os.Rename(stage, target); err != nil {
			_ = os.Rename(backup, target)
			return err
		}
		return os.RemoveAll(backup)
	}
	return os.Rename(stage, target)
}
func (b *Backend) Remove(ctx context.Context, root string, s mount.Skill) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	target := filepath.Join(root, s.Name)
	owned, err := readMarker(target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || owned.CapabilityID != s.CapabilityID {
		return ErrConflict
	}
	return os.RemoveAll(target)
}
func readMarker(dir string) (mount.Skill, error) {
	var s mount.Skill
	data, err := os.ReadFile(filepath.Join(dir, marker))
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(data, &s)
	return s, err
}
