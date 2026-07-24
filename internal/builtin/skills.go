// Package builtin embeds the built-in "using-ninea" Agent Skill and its
// authoring guides, exposing them as mount snapshots so a fresh workspace has
// a bootstrap path before other Skills are projected.
package builtin

import (
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/gopact-ai/9a/internal/mount"
)

//go:embed skills/using-ninea
var skillFS embed.FS

func UsingNineA(buildVersion string) (mount.Snapshot, error) {
	var files []mount.File
	err := fs.WalkDir(skillFS, "skills/using-ninea", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := skillFS.ReadFile(path)
		if err != nil {
			return err
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "skills/using-ninea/")
		files = append(files, mount.File{Path: rel, Mode: 0o444, Data: data})
		return nil
	})
	if err != nil {
		return mount.Snapshot{}, err
	}
	if buildVersion == "" {
		buildVersion = "dev"
	}
	return mount.NewSnapshot("builtin/using-ninea", "using-ninea", buildVersion, 0, files)
}

// ConnectionGuide returns the embedded authoring contract used by the gateway
// Skill. It gives a fresh workspace a bootstrap path before that Skill has been
// projected.
func ConnectionGuide(kind string) ([]byte, error) {
	path := ""
	switch kind {
	case "http":
		path = "skills/using-ninea/references/manifest.md"
	case "mcp", "a2a":
		path = "skills/using-ninea/references/integrations.md"
	default:
		return nil, fmt.Errorf("unsupported integration type %q: expected http, mcp, or a2a", kind)
	}
	data, err := skillFS.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s guide: %w", kind, err)
	}
	return data, nil
}
