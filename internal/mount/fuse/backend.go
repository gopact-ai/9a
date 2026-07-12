//go:build linux || darwin

package fusemount

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/gopact-ai/9a/internal/mount"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type activeMount struct {
	server     *fuse.Server
	snapshot   mount.Snapshot
	attachment mount.Attachment
}

var ErrConflict = errors.New("fuse projection conflicts with existing path")

type Backend struct {
	mu     sync.Mutex
	active map[string]*activeMount
}

func New() *Backend { return &Backend{active: map[string]*activeMount{}} }
func (*Backend) Available(context.Context) error {
	if runtime.GOOS == "linux" {
		if _, err := os.Stat("/dev/fuse"); err != nil {
			return fmt.Errorf("/dev/fuse unavailable: %w", err)
		}
		return nil
	}
	if _, err := os.Stat("/Library/Filesystems/macfuse.fs"); err != nil {
		return fmt.Errorf("macFUSE unavailable: %w", err)
	}
	return nil
}
func (b *Backend) Attach(ctx context.Context, root, workspaceID string, s mount.Snapshot) (mount.Attachment, error) {
	if err := ctx.Err(); err != nil {
		return mount.Attachment{}, err
	}
	target := filepath.Join(root, s.Name)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active[target] != nil {
		return mount.Attachment{}, fmt.Errorf("mount already active: %s", target)
	}
	if _, err := os.Lstat(target); err == nil {
		return mount.Attachment{}, ErrConflict
	} else if !os.IsNotExist(err) {
		return mount.Attachment{}, err
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		return mount.Attachment{}, err
	}
	zero := time.Duration(0)
	opts := &fs.Options{MountOptions: fuse.MountOptions{Options: []string{"ro"}, FsName: "9a", Name: "ninea"}, EntryTimeout: &zero, AttrTimeout: &zero, NegativeTimeout: &zero}
	server, err := fs.Mount(target, &roDir{tree: buildTree(s)}, opts)
	if err != nil {
		_ = os.Remove(target)
		return mount.Attachment{}, err
	}
	a := mount.Attachment{WorkspaceID: workspaceID, LogicalID: s.LogicalID, Target: target}
	b.active[target] = &activeMount{server, s, a}
	return a, nil
}
func (b *Backend) Update(ctx context.Context, a mount.Attachment, s mount.Snapshot) (mount.Attachment, error) {
	b.mu.Lock()
	old := b.active[a.Target]
	b.mu.Unlock()
	if old == nil {
		return a, errors.New("fuse mount is not active")
	}
	if err := b.Detach(ctx, a); err != nil {
		return a, err
	}
	next, err := b.Attach(ctx, filepath.Dir(a.Target), a.WorkspaceID, s)
	if err != nil {
		_, restoreErr := b.Attach(context.Background(), filepath.Dir(a.Target), a.WorkspaceID, old.snapshot)
		return a, errors.Join(err, restoreErr)
	}
	return next, nil
}
func (b *Backend) Inspect(ctx context.Context, a mount.Attachment, s mount.Snapshot) (mount.Inspection, error) {
	if err := ctx.Err(); err != nil {
		return mount.Inspection{}, err
	}
	b.mu.Lock()
	active := b.active[a.Target]
	b.mu.Unlock()
	if active == nil {
		return mount.Inspection{State: mount.InspectionMissing, Reason: "mount is not active"}, nil
	}
	if active.snapshot.Digest != s.Digest {
		return mount.Inspection{State: mount.InspectionTampered, Reason: "snapshot differs"}, nil
	}
	return mount.Inspection{State: mount.InspectionHealthy}, nil
}
func (b *Backend) Detach(ctx context.Context, a mount.Attachment) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	b.mu.Lock()
	active := b.active[a.Target]
	if active == nil {
		b.mu.Unlock()
		return nil
	}
	if active.attachment.WorkspaceID != a.WorkspaceID || active.attachment.LogicalID != a.LogicalID {
		b.mu.Unlock()
		return errors.New("fuse attachment identity mismatch")
	}
	delete(b.active, a.Target)
	b.mu.Unlock()
	if err := active.server.Unmount(); err != nil {
		b.mu.Lock()
		b.active[a.Target] = active
		b.mu.Unlock()
		return err
	}
	return os.Remove(a.Target)
}
func (b *Backend) Close(ctx context.Context) error {
	b.mu.Lock()
	targets := make([]string, 0, len(b.active))
	for target := range b.active {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	attachments := make([]mount.Attachment, 0, len(targets))
	for _, target := range targets {
		attachments = append(attachments, b.active[target].attachment)
	}
	b.mu.Unlock()
	var result error
	for _, a := range attachments {
		result = errors.Join(result, b.Detach(ctx, a))
	}
	return result
}
