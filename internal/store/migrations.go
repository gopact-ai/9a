package store

import (
	"context"
	"database/sql"
	"fmt"
)

const schema = `
CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value INTEGER NOT NULL);
INSERT OR IGNORE INTO metadata(key,value) VALUES ('catalog_revision',0);
CREATE TABLE IF NOT EXISTS external_adapters (
 protocol TEXT PRIMARY KEY, executable TEXT NOT NULL, created_at TEXT NOT NULL
);
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
DROP TABLE IF EXISTS projections;
CREATE TABLE IF NOT EXISTS workspaces (
 id TEXT PRIMARY KEY, root TEXT NOT NULL UNIQUE, skills_root TEXT NOT NULL,
 policy TEXT NOT NULL, backend TEXT NOT NULL, state TEXT NOT NULL,
 fallback_reason TEXT NOT NULL DEFAULT '', format INTEGER NOT NULL,
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
CREATE TABLE IF NOT EXISTS usage (identity_id TEXT NOT NULL, capability_id TEXT NOT NULL, successes INTEGER NOT NULL DEFAULT 0, failures INTEGER NOT NULL DEFAULT 0, last_used_at TEXT, PRIMARY KEY(identity_id,capability_id));
INSERT INTO call_storage_usage(call_id,byte_count)
SELECT c.id,
       coalesce(length(i.data_json),0) +
       coalesce(length(r.data_json),0) +
       coalesce((SELECT sum(length(e.data_json)) FROM events e WHERE e.call_id=c.id),0)
FROM calls c
LEFT JOIN call_inputs i ON i.call_id=c.id
LEFT JOIN call_results r ON r.call_id=c.id
ON CONFLICT(call_id) DO UPDATE SET byte_count=excluded.byte_count;
`

func migrate(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("migrate sqlite: %w", err)
	}
	return nil
}
