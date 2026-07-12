package store

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenAppliesMigrations(t *testing.T) {
	t.Parallel()
	db, err := Open(context.Background(), t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, table := range []string{"external_adapters", "providers", "capabilities", "capability_fts", "acl", "workspaces", "managed_skills", "calls", "call_inputs", "call_results", "call_event_usage", "call_storage_usage", "events", "usage"} {
		var name string
		if err := db.QueryRow(`SELECT name FROM sqlite_master WHERE name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("missing %s: %v", table, err)
		}
	}
}

func TestOpenBackfillsCallStorageUsage(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ninea.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO calls(id,capability_id,identity_id,state,created_at,updated_at) VALUES('call-existing','echo/demo/echo','agent','completed','2026-07-12T00:00:00Z','2026-07-12T00:00:00Z');
INSERT INTO call_inputs(call_id,data_json) VALUES('call-existing','{"in":1}');
INSERT INTO call_results(call_id,data_json) VALUES('call-existing','{"out":2}');
INSERT INTO events(call_id,sequence,data_json) VALUES('call-existing',1,'{"event":3}');
`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	var got int64
	if err := db.QueryRowContext(ctx, `SELECT byte_count FROM call_storage_usage WHERE call_id='call-existing'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	want := int64(len(`{"in":1}`) + len(`{"out":2}`) + len(`{"event":3}`))
	if got != want {
		t.Fatalf("byte_count=%d want=%d", got, want)
	}
}
