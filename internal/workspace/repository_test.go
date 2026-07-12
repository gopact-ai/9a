package workspace

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/store"
)

func TestRepositoryWorkspaceAndManagedSkillLifecycle(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewRepository(db)
	w := Workspace{ID: "ws-1", Root: "/workspace", SkillsRoot: "/workspace/.agents/skills", Policy: PolicyAuto, Backend: BackendDirectory, State: StateFallback, FallbackReason: "fuse unavailable", Format: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := repo.PutWorkspace(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetWorkspaceByRoot(context.Background(), w.Root)
	if err != nil || got.ID != w.ID || got.Backend != BackendDirectory {
		t.Fatalf("workspace=%#v err=%v", got, err)
	}
	s := ManagedSkill{WorkspaceID: w.ID, LogicalID: "builtin/using-ninea", TargetName: "using-ninea", SourceKind: "builtin", SourceID: "using-ninea", CatalogRevision: 1, SkillVersion: "v1", Digest: "abc", MountState: "attached", UpdatedAt: time.Now().UTC()}
	if err := repo.PutManagedSkill(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	items, err := repo.ListManagedSkills(context.Background(), w.ID)
	if err != nil || len(items) != 1 || items[0].LogicalID != s.LogicalID {
		t.Fatalf("skills=%#v err=%v", items, err)
	}
	if err := repo.DeleteManagedSkill(context.Background(), w.ID, s.LogicalID); err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteWorkspace(context.Background(), w.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.GetWorkspaceByRoot(context.Background(), w.Root); err == nil {
		t.Fatal("deleted workspace found")
	}
}

func TestRepositoryRejectsDuplicateWorkspaceRoot(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	repo := NewRepository(db)
	for _, id := range []string{"one", "two"} {
		err = repo.PutWorkspace(context.Background(), Workspace{ID: id, Root: "/same", SkillsRoot: "/same/.agents/skills", Policy: PolicyDirectory, Backend: BackendDirectory, State: StateHealthy, Format: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()})
		if id == "one" && err != nil {
			t.Fatal(err)
		}
	}
	if err == nil {
		t.Fatal("duplicate workspace root accepted")
	}
}
