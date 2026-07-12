package dir

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gopact-ai/9a/internal/mount"
)

const marker = ".ninea-owned.json"
const manifestVersion = 1

var ErrConflict = errors.New("projection conflicts with unowned directory")

type fileRecord struct {
	Path   string `json:"path"`
	Mode   uint32 `json:"mode"`
	Size   int    `json:"size"`
	SHA256 string `json:"sha256"`
}
type manifest struct {
	Version         int          `json:"version"`
	WorkspaceID     string       `json:"workspaceId"`
	LogicalID       string       `json:"logicalId"`
	Name            string       `json:"name"`
	SkillVersion    string       `json:"skillVersion"`
	CatalogRevision int64        `json:"catalogRevision"`
	Digest          string       `json:"digest"`
	Files           []fileRecord `json:"files"`
}
type Backend struct{}

func New() *Backend { return &Backend{} }

func records(s mount.Snapshot) []fileRecord {
	out := make([]fileRecord, 0, len(s.Files))
	for _, f := range s.Files {
		sum := sha256.Sum256(f.Data)
		out = append(out, fileRecord{f.Path, uint32(f.Mode.Perm()), len(f.Data), hex.EncodeToString(sum[:])})
	}
	return out
}
func expectedManifest(workspaceID string, s mount.Snapshot) manifest {
	return manifest{manifestVersion, workspaceID, s.LogicalID, s.Name, s.Version, s.CatalogRevision, s.Digest, records(s)}
}
func readManifest(target string) (manifest, error) {
	var m manifest
	data, err := os.ReadFile(filepath.Join(target, marker))
	if err != nil {
		return m, err
	}
	err = json.Unmarshal(data, &m)
	return m, err
}
func sameIdentity(m manifest, a mount.Attachment) bool {
	return m.Version == manifestVersion && m.WorkspaceID == a.WorkspaceID && m.LogicalID == a.LogicalID && filepath.Base(a.Target) == m.Name
}

func (b *Backend) Attach(ctx context.Context, root, workspaceID string, s mount.Snapshot) (mount.Attachment, error) {
	a := mount.Attachment{WorkspaceID: workspaceID, LogicalID: s.LogicalID, Target: filepath.Join(root, s.Name)}
	if err := b.publish(ctx, root, a, s); err != nil {
		return mount.Attachment{}, err
	}
	return a, nil
}
func (b *Backend) Update(ctx context.Context, a mount.Attachment, s mount.Snapshot) (mount.Attachment, error) {
	if a.LogicalID != s.LogicalID {
		return a, ErrConflict
	}
	root := filepath.Dir(a.Target)
	if err := b.publish(ctx, root, a, s); err != nil {
		return a, err
	}
	return a, nil
}

func (b *Backend) publish(ctx context.Context, root string, a mount.Attachment, s mount.Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if filepath.Join(root, s.Name) != a.Target {
		return ErrConflict
	}
	if info, err := os.Lstat(a.Target); err == nil {
		if !info.IsDir() {
			return ErrConflict
		}
		m, e := readManifest(a.Target)
		if e != nil || !sameIdentity(m, a) {
			return ErrConflict
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(root, ".ninea-stage-")
	if err != nil {
		return err
	}
	defer func() { _ = makeWritable(stage); _ = os.RemoveAll(stage) }()
	dirs := map[string]bool{stage: true}
	for _, f := range s.Files {
		path := filepath.Join(stage, filepath.FromSlash(f.Path))
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		for d := dir; strings.HasPrefix(d, stage); d = filepath.Dir(d) {
			dirs[d] = true
			if d == stage {
				break
			}
		}
		if err := os.WriteFile(path, f.Data, f.Mode); err != nil {
			return err
		}
		if err := os.Chmod(path, f.Mode); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(expectedManifest(a.WorkspaceID, s), "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(stage, marker), data, 0o400); err != nil {
		return err
	}
	if err := os.Chmod(filepath.Join(stage, marker), 0o400); err != nil {
		return err
	}
	ordered := make([]string, 0, len(dirs))
	for d := range dirs {
		ordered = append(ordered, d)
	}
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, d := range ordered {
		if d == stage {
			continue
		}
		if err := os.Chmod(d, 0o555); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(a.Target); err == nil {
		backup, err := os.MkdirTemp(root, ".ninea-backup-")
		if err != nil {
			return err
		}
		if err = os.Remove(backup); err != nil {
			return err
		}
		if err = os.Chmod(a.Target, 0o755); err != nil {
			return err
		}
		if err = os.Rename(a.Target, backup); err != nil {
			_ = os.Chmod(a.Target, 0o555)
			return err
		}
		if err = os.Rename(stage, a.Target); err != nil {
			_ = os.Rename(backup, a.Target)
			return err
		}
		if err := os.Chmod(a.Target, 0o555); err != nil {
			return err
		}
		_ = makeWritable(backup)
		return os.RemoveAll(backup)
	}
	if err := os.Rename(stage, a.Target); err != nil {
		return err
	}
	return os.Chmod(a.Target, 0o555)
}

func (b *Backend) Inspect(ctx context.Context, a mount.Attachment, s mount.Snapshot) (mount.Inspection, error) {
	if err := ctx.Err(); err != nil {
		return mount.Inspection{}, err
	}
	m, err := readManifest(a.Target)
	if os.IsNotExist(err) {
		return mount.Inspection{State: mount.InspectionMissing, Reason: "target missing"}, nil
	}
	if err != nil || !sameIdentity(m, a) {
		return mount.Inspection{State: mount.InspectionTampered, Reason: "invalid ownership manifest"}, nil
	}
	want := expectedManifest(a.WorkspaceID, s)
	if m.Digest != want.Digest || len(m.Files) != len(want.Files) {
		return mount.Inspection{State: mount.InspectionTampered, Reason: "manifest differs"}, nil
	}
	expectedFiles := map[string]os.FileMode{marker: 0o400}
	expectedDirs := map[string]bool{".": true}
	for _, f := range want.Files {
		expectedFiles[filepath.FromSlash(f.Path)] = os.FileMode(f.Mode)
		for parent := filepath.Dir(filepath.FromSlash(f.Path)); parent != "."; parent = filepath.Dir(parent) {
			expectedDirs[parent] = true
		}
	}
	if walkErr := filepath.WalkDir(a.Target, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, relErr := filepath.Rel(a.Target, path)
		if relErr != nil {
			return relErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink %s", rel)
		}
		info, infoErr := entry.Info()
		if infoErr != nil {
			return infoErr
		}
		if entry.IsDir() {
			if !expectedDirs[rel] || info.Mode().Perm() != 0o555 {
				return fmt.Errorf("unexpected directory %s", rel)
			}
			return nil
		}
		mode, ok := expectedFiles[rel]
		if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
			return fmt.Errorf("unexpected file %s", rel)
		}
		return nil
	}); walkErr != nil {
		return mount.Inspection{State: mount.InspectionTampered, Reason: walkErr.Error()}, nil
	}
	for _, f := range want.Files {
		path := filepath.Join(a.Target, filepath.FromSlash(f.Path))
		info, e := os.Stat(path)
		if e != nil || uint32(info.Mode().Perm()) != f.Mode || int(info.Size()) != f.Size {
			return mount.Inspection{State: mount.InspectionTampered, Reason: "file metadata differs"}, nil
		}
		data, e := os.ReadFile(path)
		if e != nil {
			return mount.Inspection{}, e
		}
		sum := sha256.Sum256(data)
		if hex.EncodeToString(sum[:]) != f.SHA256 {
			return mount.Inspection{State: mount.InspectionTampered, Reason: "file digest differs"}, nil
		}
	}
	return mount.Inspection{State: mount.InspectionHealthy}, nil
}
func makeWritable(root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o700)
		}
		return nil
	})
}
func (b *Backend) Detach(ctx context.Context, a mount.Attachment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	m, err := readManifest(a.Target)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !sameIdentity(m, a) {
		return ErrConflict
	}
	if err := makeWritable(a.Target); err != nil {
		return err
	}
	return os.RemoveAll(a.Target)
}

// Publish and Remove preserve the v0 API while callers migrate to managed attachments.
func (b *Backend) Publish(ctx context.Context, root string, s mount.Skill) error {
	snap, err := mount.NewSnapshot(s.CapabilityID, s.Name, fmt.Sprintf("%d", s.Revision), s.Revision, s.Files)
	if err != nil {
		return err
	}
	_, err = b.Attach(ctx, root, "legacy", snap)
	return err
}
func (b *Backend) Remove(ctx context.Context, root string, s mount.Skill) error {
	return b.Detach(ctx, mount.Attachment{WorkspaceID: "legacy", LogicalID: s.CapabilityID, Target: filepath.Join(root, s.Name)})
}
