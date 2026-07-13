package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/store"
	"github.com/gopact-ai/9a/internal/workspace"
)

func TestUpdateRequiresAdminAndRepairsBuiltInSkill(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := New(db)
	if err = a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	if _, err = a.AttachWorkspace(ctx, "admin", root, workspace.PolicyDirectory); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, ".agents", "skills", "using-ninea", "SKILL.md")
	if err = os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte("tampered"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err = a.UpdateWorkspaces(ctx, "user", root, false, false); err == nil {
		t.Fatal("non-admin update accepted")
	}
	check, err := a.UpdateWorkspaces(ctx, "admin", root, true, false)
	if err != nil {
		t.Fatal(err)
	}
	if check.Workspaces[0].Repaired != 1 {
		t.Fatalf("check=%#v", check)
	}
	if data, _ := os.ReadFile(path); string(data) != "tampered" {
		t.Fatal("check mutated projection")
	}
	result, err := a.UpdateWorkspaces(ctx, "admin", root, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Workspaces) != 1 || result.Workspaces[0].Repaired != 1 {
		t.Fatalf("result=%#v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) == "tampered" {
		t.Fatalf("repair=%q err=%v", data, err)
	}
}

func TestUpdateDoesNotAdvanceCatalogForUnchangedProvider(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := New(db)
	if err = a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", server.URL, 1))
	if _, err = a.AddDeclarative(ctx, "admin", source, root); err != nil {
		t.Fatal(err)
	}
	before, err := catalog.New(db).Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := a.UpdateWorkspaces(ctx, "admin", root, false, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := catalog.New(db).Revision(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("revision changed from %d to %d", before, after)
	}
	if len(result.Providers) != 1 || result.Providers[0].State != "unchanged" {
		t.Fatalf("result=%#v", result)
	}
}
