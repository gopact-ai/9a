package app

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/gopact-ai/9a/internal/store"
)

func testApp(t *testing.T) (*App, *sql.DB) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db), db
}

func approvalForRun(t *testing.T, a *App, identity, root, ref string, input json.RawMessage) string {
	t.Helper()
	_, err := a.RunInWorkspace(context.Background(), identity, root, ref, input, "")
	var approval *ApprovalRequiredError
	if !errors.As(err, &approval) || approval.Token == "" {
		t.Fatalf("approval preflight for %s error=%#v", ref, err)
	}
	return approval.Token
}
