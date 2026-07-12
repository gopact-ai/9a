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
	return mount.NewSnapshot("builtin/using-ninea", "using-ninea", fmt.Sprintf("%s", buildVersion), 0, files)
}
