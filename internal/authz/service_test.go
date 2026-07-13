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

func TestGrantIfAbsentReportsOwnership(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	created, err := s.GrantIfAbsent(ctx, "agent", "cap", Invoke)
	if err != nil || !created {
		t.Fatalf("first created=%v err=%v", created, err)
	}
	created, err = s.GrantIfAbsent(ctx, "agent", "cap", Invoke)
	if err != nil || created {
		t.Fatalf("second created=%v err=%v", created, err)
	}
}

func TestGrantRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	for _, test := range []struct {
		identity, capability string
		permission           Permission
	}{
		{"", "cap", Read},
		{"agent", "", Read},
		{"agent", "cap", Permission("invkoe")},
	} {
		if err := s.Grant(ctx, test.identity, test.capability, test.permission); err == nil {
			t.Fatalf("Grant(%q, %q, %q) accepted invalid input", test.identity, test.capability, test.permission)
		}
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM acl`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("invalid grants persisted %d ACL rows", count)
	}
}
