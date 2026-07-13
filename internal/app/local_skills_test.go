package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/search"
	"github.com/gopact-ai/9a/internal/store"
	"github.com/gopact-ai/9a/internal/workspace"
)

func TestSearchIndexesLocalSkillsAndTracksDirectoryChanges(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := New(db)
	if err = a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	if _, err = a.AttachWorkspace(ctx, root, workspace.PolicyDirectory); err != nil {
		t.Fatal(err)
	}
	results, err := a.Search(ctx, "admin", search.Query{Text: "using-ninea"})
	if err != nil || len(results) != 0 {
		t.Fatalf("managed Skill was indexed as local: %#v err=%v", results, err)
	}

	skillRoot := filepath.Join(root, ".agents", "skills", "weather")
	if err = os.MkdirAll(skillRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill := func(description string) {
		t.Helper()
		data := []byte("---\nname: local-weather\ndescription: " + description + "\n---\n\n# Weather\n")
		if writeErr := os.WriteFile(filepath.Join(skillRoot, "SKILL.md"), data, 0o644); writeErr != nil {
			t.Fatal(writeErr)
		}
	}
	writeSkill("current temperature")
	results, err = a.Search(ctx, "admin", search.Query{Text: "temperature"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Capability.Name != "local-weather" {
		t.Fatalf("results=%#v", results)
	}
	otherRoot := t.TempDir()
	cleanupReadOnlyProjection(t, otherRoot)
	otherSkill := filepath.Join(otherRoot, ".agents", "skills", "calendar")
	if err = os.MkdirAll(otherSkill, 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(otherSkill, "SKILL.md"), []byte("---\nname: local-calendar\ndescription: shared calendar\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = a.AttachWorkspace(ctx, otherRoot, workspace.PolicyDirectory); err != nil {
		t.Fatal(err)
	}
	results, err = a.Search(ctx, "admin", search.Query{Text: "local skill"})
	if err != nil || len(results) != 2 {
		t.Fatalf("fused workspace results=%#v err=%v", results, err)
	}

	revision, err := catalog.New(db).Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(skillRoot, "references"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(skillRoot, "references", "notes.md"), []byte("changed"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Search(ctx, "admin", search.Query{Text: "temperature"}); err != nil {
		t.Fatal(err)
	}
	changedRevision, err := catalog.New(db).Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if changedRevision <= revision {
		t.Fatalf("directory change did not refresh catalog: %d -> %d", revision, changedRevision)
	}
	if _, err = a.Search(ctx, "admin", search.Query{Text: "temperature"}); err != nil {
		t.Fatal(err)
	}
	unchangedRevision, err := catalog.New(db).Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if unchangedRevision != changedRevision {
		t.Fatalf("unchanged scan advanced catalog: %d -> %d", changedRevision, unchangedRevision)
	}

	writeSkill("humidity monitor")
	results, err = a.Search(ctx, "admin", search.Query{Text: "humidity"})
	if err != nil || len(results) != 1 {
		t.Fatalf("modified skill results=%#v err=%v", results, err)
	}
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	a = New(db)
	if err = a.Restore(ctx); err != nil {
		t.Fatal(err)
	}

	if err = os.RemoveAll(skillRoot); err != nil {
		t.Fatal(err)
	}
	results, err = a.Search(ctx, "admin", search.Query{Text: "humidity"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("removed skill remains searchable: %#v", results)
	}

	if err = os.MkdirAll(skillRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	writeSkill("detach cleanup")
	results, err = a.Search(ctx, "admin", search.Query{Text: "detach"})
	if err != nil || len(results) != 1 {
		t.Fatalf("detach setup results=%#v err=%v", results, err)
	}
	detachedID := results[0].Capability.ID
	if err = a.Grant(ctx, "agent", detachedID, []string{"write"}); err != nil {
		t.Fatal(err)
	}
	if err = a.DetachWorkspace(ctx, root); err != nil {
		t.Fatal(err)
	}
	if !a.az.Allowed(ctx, "agent", detachedID, authz.Write) {
		t.Fatal("detach removed local Skill ACL")
	}
	results, err = a.Search(ctx, "admin", search.Query{Text: "detach"})
	if err != nil || len(results) != 0 {
		t.Fatalf("detached Skill remains searchable: %#v err=%v", results, err)
	}
}

func TestLocalSkillIDPreservesSlugCollisions(t *testing.T) {
	if localSkillID("ws-test", "foo-bar") == localSkillID("ws-test", "foo_bar") {
		t.Fatal("distinct directory names produced the same capability ID")
	}
}
