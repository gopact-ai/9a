package fusemount

import (
	"context"
	"github.com/gopact-ai/9a/internal/mount"
	"github.com/hanwen/go-fuse/v2/fuse"
	"syscall"
	"testing"
)

func TestReadOnlyNodesRejectMutation(t *testing.T) {
	snapshot, _ := mount.NewSnapshot("id", "skill", "v1", 1, []mount.File{{Path: "refs/a.txt", Data: []byte("hello"), Mode: 0o644}})
	tree := buildTree(snapshot)
	if tree.dirs["refs"].files["a.txt"].Path != "refs/a.txt" {
		t.Fatal("tree missing file")
	}
	dir := &roDir{tree: tree}
	if got := dir.Unlink(context.Background(), "refs"); got != syscall.EROFS {
		t.Fatalf("unlink=%v", got)
	}
	file := &roFile{data: []byte("hello"), mode: 0o444}
	if _, got := file.Write(context.Background(), nil, []byte("x"), 0); got != syscall.EROFS {
		t.Fatalf("write=%v", got)
	}
	out := &fuse.AttrOut{}
	if got := file.Setattr(context.Background(), nil, &fuse.SetAttrIn{}, out); got != syscall.EROFS {
		t.Fatalf("setattr=%v", got)
	}
}
