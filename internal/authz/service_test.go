package authz

import (
	"context"
	"testing"

	"github.com/gopact-ai/9a/internal/store"
)

func TestDefaultDenyAndSeparatePermissions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db)
	if s.Allowed(ctx, "agent", "cap", Read) {
		t.Fatal("default allow")
	}
	if err := s.Grant(ctx, "agent", "cap", Read); err != nil {
		t.Fatal(err)
	}
	if !s.Allowed(ctx, "agent", "cap", Read) {
		t.Fatal("read grant ignored")
	}
	if s.Allowed(ctx, "agent", "cap", Invoke) {
		t.Fatal("read implied invoke")
	}
}

func TestGlobalAdminIsExplicitAndDoesNotImplyCapabilityRead(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db)
	if err := s.Grant(ctx, "root", "*", Admin); err != nil {
		t.Fatal(err)
	}
	if !s.Allowed(ctx, "root", "*", Admin) {
		t.Fatal("admin grant missing")
	}
	if s.Allowed(ctx, "root", "cap", Read) {
		t.Fatal("admin implicitly exposed capability")
	}
}
