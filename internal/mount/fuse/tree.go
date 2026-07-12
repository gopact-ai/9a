package fusemount

import (
	"context"
	"path"
	"sort"
	"strings"
	"syscall"

	"github.com/gopact-ai/9a/internal/mount"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
)

type treeDir struct {
	dirs  map[string]*treeDir
	files map[string]mount.File
}

func buildTree(snapshot mount.Snapshot) *treeDir {
	root := &treeDir{map[string]*treeDir{}, map[string]mount.File{}}
	for _, file := range snapshot.Files {
		parts := strings.Split(path.Clean(file.Path), "/")
		current := root
		for _, part := range parts[:len(parts)-1] {
			next := current.dirs[part]
			if next == nil {
				next = &treeDir{map[string]*treeDir{}, map[string]mount.File{}}
				current.dirs[part] = next
			}
			current = next
		}
		current.files[parts[len(parts)-1]] = file
	}
	return root
}

type roDir struct {
	fs.Inode
	tree *treeDir
}

func (d *roDir) OnAdd(ctx context.Context) {
	names := make([]string, 0, len(d.tree.dirs)+len(d.tree.files))
	for n := range d.tree.dirs {
		names = append(names, n)
	}
	for n := range d.tree.files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		var child *fs.Inode
		if subtree := d.tree.dirs[name]; subtree != nil {
			child = d.NewPersistentInode(ctx, &roDir{tree: subtree}, fs.StableAttr{Mode: syscall.S_IFDIR})
		} else {
			file := d.tree.files[name]
			child = d.NewPersistentInode(ctx, &roFile{data: file.Data, mode: uint32(file.Mode.Perm())}, fs.StableAttr{Mode: syscall.S_IFREG})
		}
		d.AddChild(name, child, false)
	}
}
func (*roDir) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFDIR | 0o555
	return 0
}
func (*roDir) Setattr(context.Context, fs.FileHandle, *fuse.SetAttrIn, *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}
func (*roDir) Mkdir(context.Context, string, uint32, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}
func (*roDir) Mknod(context.Context, string, uint32, uint32, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}
func (*roDir) Create(context.Context, string, uint32, uint32, *fuse.EntryOut) (*fs.Inode, fs.FileHandle, uint32, syscall.Errno) {
	return nil, nil, 0, syscall.EROFS
}
func (*roDir) Unlink(context.Context, string) syscall.Errno { return syscall.EROFS }
func (*roDir) Rmdir(context.Context, string) syscall.Errno  { return syscall.EROFS }
func (*roDir) Rename(context.Context, string, fs.InodeEmbedder, string, uint32) syscall.Errno {
	return syscall.EROFS
}
func (*roDir) Symlink(context.Context, string, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}
func (*roDir) Link(context.Context, fs.InodeEmbedder, string, *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	return nil, syscall.EROFS
}
func (*roDir) Setxattr(context.Context, string, []byte, uint32) syscall.Errno { return syscall.EROFS }
func (*roDir) Removexattr(context.Context, string) syscall.Errno              { return syscall.EROFS }

type roFile struct {
	fs.Inode
	data []byte
	mode uint32
}

func (f *roFile) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = syscall.S_IFREG | f.mode
	out.Size = uint64(len(f.data))
	return 0
}
func (*roFile) Setattr(context.Context, fs.FileHandle, *fuse.SetAttrIn, *fuse.AttrOut) syscall.Errno {
	return syscall.EROFS
}
func (*roFile) Write(context.Context, fs.FileHandle, []byte, int64) (uint32, syscall.Errno) {
	return 0, syscall.EROFS
}
func (*roFile) Setxattr(context.Context, string, []byte, uint32) syscall.Errno { return syscall.EROFS }
func (*roFile) Removexattr(context.Context, string) syscall.Errno              { return syscall.EROFS }
func (*roFile) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&3 != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_DIRECT_IO, 0
}
func (f *roFile) Read(_ context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	if off < 0 || off >= int64(len(f.data)) {
		return fuse.ReadResultData(nil), 0
	}
	end := off + int64(len(dest))
	if end > int64(len(f.data)) {
		end = int64(len(f.data))
	}
	return fuse.ReadResultData(f.data[off:end]), 0
}
