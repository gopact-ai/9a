package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenInitializesFreshDatabase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})

	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version != schemaVersion {
		t.Fatalf("schema version=%d want %d", version, schemaVersion)
	}

	for _, table := range []string{"providers", "capabilities", "capability_fts", "acl", "workspaces", "managed_skills", "calls", "call_inputs", "call_results", "call_event_usage", "call_storage_usage", "events"} {
		var name string
		if err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_schema WHERE name = ?`, table).Scan(&name); err != nil {
			t.Fatalf("missing %s: %v", table, err)
		}
	}

	var usageTables int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'usage'`).Scan(&usageTables); err != nil {
		t.Fatal(err)
	}
	if usageTables != 0 {
		t.Fatal("unused usage table was created")
	}
}

func TestOpenAcceptsCurrentSchemaVersion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ninea.db")
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO metadata(key, value) VALUES ('reopen_marker', 42)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	var marker int
	if err := db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key = 'reopen_marker'`).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != 42 {
		t.Fatalf("reopen marker=%d want 42", marker)
	}
}

func TestOpenSecuresDatabaseAndWALSidecars(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "ninea.db")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	db, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('permission_marker',1)`); err != nil {
		t.Fatal(err)
	}
	assertSQLiteFileModes(t, path)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.ExecContext(ctx, `INSERT INTO metadata(key,value) VALUES('reopen_permission_marker',1)`); err != nil {
		t.Fatal(err)
	}
	assertSQLiteFileModes(t, path)
}

func assertSQLiteFileModes(t *testing.T, path string) {
	t.Helper()
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatalf("stat %s: %v", filepath.Base(candidate), err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("%s mode=%o want 600", filepath.Base(candidate), got)
		}
	}
}

func TestOpenRejectsIncompatibleNonemptyDatabaseWithoutMigration(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name    string
		version int
	}{
		{name: "unversioned", version: 0},
		{name: "unknown version", version: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "ninea.db")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, `CREATE TABLE legacy_state (value TEXT NOT NULL)`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, test.version)); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}

			opened, err := Open(ctx, path)
			if opened != nil {
				if closeErr := opened.Close(); closeErr != nil {
					t.Errorf("close unexpectedly opened database: %v", closeErr)
				}
			}
			if !errors.Is(err, ErrIncompatibleState) {
				t.Fatalf("Open error=%v want ErrIncompatibleState", err)
			}
			if !strings.Contains(err.Error(), fmt.Sprintf("schema version %d", test.version)) {
				t.Fatalf("Open error=%q does not identify schema version", err)
			}
			for _, want := range []string{path, "move", "remove", "restart 9a"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("Open error=%q does not provide next step %q", err, want)
				}
			}

			db, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := db.Close(); err != nil {
					t.Errorf("close database: %v", err)
				}
			})
			var currentVersion, applicationTables, legacyTables int
			if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&currentVersion); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'providers'`).Scan(&applicationTables); err != nil {
				t.Fatal(err)
			}
			if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table' AND name = 'legacy_state'`).Scan(&legacyTables); err != nil {
				t.Fatal(err)
			}
			if currentVersion != test.version || applicationTables != 0 || legacyTables != 1 {
				t.Fatalf("rejected database changed: version=%d providers=%d legacy=%d", currentVersion, applicationTables, legacyTables)
			}
		})
	}
}
