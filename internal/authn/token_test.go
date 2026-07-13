package authn

import (
	"context"
	"strings"
	"testing"

	"github.com/gopact-ai/9a/internal/store"
)

func TestNewTokenReturnsUniqueNineaTokens(t *testing.T) {
	t.Parallel()
	first, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "ninea_") || !strings.HasPrefix(second, "ninea_") {
		t.Fatalf("NewToken() returned %q and %q", first, second)
	}
	if first == second {
		t.Fatalf("NewToken() returned duplicate token %q", first)
	}
}

func TestCreateAndAuthenticateStoresOnlyHash(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db)
	token, err := s.Create(ctx, "agent-1")
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("empty token")
	}
	if identity, err := s.Authenticate(ctx, token); err != nil || identity != "agent-1" {
		t.Fatalf("Authenticate = %q, %v", identity, err)
	}
	var count int
	if err := db.QueryRow(`SELECT count(*) FROM tokens WHERE token_hash=?`, token).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("plaintext token persisted")
	}
}

func TestImportBootstrapTokenOnlyIntoEmptyStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db)
	if err := s.Import(ctx, "bootstrap-secret", "admin"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Authenticate(ctx, "bootstrap-secret"); err != nil || got != "admin" {
		t.Fatalf("identity=%q err=%v", got, err)
	}
	if err := s.Import(ctx, "second-secret", "attacker"); err != ErrAlreadyBootstrapped {
		t.Fatalf("second import err=%v", err)
	}
}
