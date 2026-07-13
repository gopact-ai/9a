package app

import (
	"context"
	"github.com/gopact-ai/9a/internal/authn"
	"github.com/gopact-ai/9a/internal/store"
	"strings"
	"testing"
)

func TestBootstrapRollsBackTokenWhenAdminGrantFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`CREATE TRIGGER reject_admin BEFORE INSERT ON acl BEGIN SELECT RAISE(FAIL,'reject admin'); END`); err != nil {
		t.Fatal(err)
	}
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err == nil {
		t.Fatal("bootstrap unexpectedly passed")
	}
	if _, err := authn.New(db).Authenticate(ctx, "secret"); err != authn.ErrInvalidToken {
		t.Fatalf("token survived rollback: %v", err)
	}
}

func TestGrantRejectsInvalidInputBeforePersisting(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := New(db)
	tests := []struct {
		identity, capability string
		permissions          []string
		want                 string
	}{
		{"", "echo/demo/echo", []string{"read"}, "identity"},
		{"agent", "", []string{"read"}, "capability"},
		{"agent", "echo/demo/echo", nil, "permission"},
		{"agent", "echo/demo/echo", []string{"read", "invkoe"}, "invkoe"},
	}
	for _, test := range tests {
		if err := a.Grant(ctx, test.identity, test.capability, test.permissions); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Fatalf("Grant(%q, %q, %v) error=%v", test.identity, test.capability, test.permissions, err)
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
