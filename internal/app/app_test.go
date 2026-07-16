package app

import (
	"context"
	"testing"

	"github.com/gopact-ai/9a/internal/authn"
	"github.com/gopact-ai/9a/internal/store"
)

func TestBootstrapRollsBackTokenWhenAdminGrantFails(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
