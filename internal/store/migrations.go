// Package store defines the SQLite schema for the 9a runtime and initializes
// it, creating the tables for providers, capabilities, ACLs, tokens, secrets,
// workspaces, managed skills, and calls.
package store

import (
	"context"
	"database/sql"
	"fmt"
)

const schemaVersion = 1

const schema = `
CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
INSERT OR IGNORE INTO metadata(key,value) VALUES ('catalog_revision',0);
CREATE TABLE IF NOT EXISTS providers (
 id TEXT PRIMARY KEY, protocol TEXT NOT NULL, name TEXT NOT NULL, endpoint TEXT NOT NULL,
 revision INTEGER NOT NULL, config_json BLOB NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS capabilities (
 id TEXT PRIMARY KEY, provider_id TEXT NOT NULL REFERENCES providers(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL, kind TEXT NOT NULL, name TEXT NOT NULL, description TEXT NOT NULL,
 protocol TEXT NOT NULL, provider_name TEXT NOT NULL, data_json BLOB NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS capability_fts USING fts5(id UNINDEXED, name, description, tags, examples);
CREATE TABLE IF NOT EXISTS acl (identity_id TEXT NOT NULL, capability_id TEXT NOT NULL, permission TEXT NOT NULL, PRIMARY KEY(identity_id,capability_id,permission));
CREATE TABLE IF NOT EXISTS tokens (token_hash TEXT PRIMARY KEY, identity_id TEXT NOT NULL, created_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS secrets (name TEXT PRIMARY KEY, created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workspaces (
 id TEXT PRIMARY KEY, root TEXT NOT NULL UNIQUE, skills_root TEXT NOT NULL,
 policy TEXT NOT NULL, backend TEXT NOT NULL, state TEXT NOT NULL,
 format INTEGER NOT NULL,
 created_at TEXT NOT NULL, updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS managed_skills (
 workspace_id TEXT NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
 logical_id TEXT NOT NULL, target_root TEXT NOT NULL, target_name TEXT NOT NULL, source_kind TEXT NOT NULL,
 source_id TEXT NOT NULL, catalog_revision INTEGER NOT NULL,
 skill_version TEXT NOT NULL, digest TEXT NOT NULL, mount_state TEXT NOT NULL,
 updated_at TEXT NOT NULL,
 PRIMARY KEY(workspace_id,logical_id), UNIQUE(workspace_id,target_root,target_name)
);
CREATE TABLE IF NOT EXISTS calls (id TEXT PRIMARY KEY, capability_id TEXT NOT NULL, identity_id TEXT NOT NULL, state TEXT NOT NULL, code TEXT NOT NULL DEFAULT '', message TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS call_inputs (call_id TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE, data_json BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS call_results (call_id TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE, data_json BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS call_event_usage (call_id TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE, event_count INTEGER NOT NULL DEFAULT 0 CHECK(event_count >= 0), byte_count INTEGER NOT NULL DEFAULT 0 CHECK(byte_count >= 0));
CREATE TABLE IF NOT EXISTS call_storage_usage (call_id TEXT PRIMARY KEY REFERENCES calls(id) ON DELETE CASCADE, byte_count INTEGER NOT NULL DEFAULT 0 CHECK(byte_count >= 0));
CREATE TABLE IF NOT EXISTS events (call_id TEXT NOT NULL REFERENCES calls(id) ON DELETE CASCADE, sequence INTEGER NOT NULL, data_json BLOB NOT NULL, PRIMARY KEY(call_id,sequence));
`

func initializeSchema(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin sqlite schema initialization: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("initialize sqlite schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `PRAGMA user_version = 1`); err != nil {
		return fmt.Errorf("set sqlite schema version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sqlite schema initialization: %w", err)
	}
	return nil
}
