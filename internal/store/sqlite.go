package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"

	_ "modernc.org/sqlite"
)

var ErrIncompatibleState = errors.New("state database is incompatible")

func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := secureSQLiteFiles(path, true); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `PRAGMA foreign_keys=ON; PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000;`); err != nil {
		return nil, errors.Join(fmt.Errorf("configure sqlite: %w", err), db.Close())
	}
	if err = prepareSchema(ctx, db); err != nil {
		closeErr := db.Close()
		if errors.Is(err, ErrIncompatibleState) {
			return nil, errors.Join(fmt.Errorf("%w; move %q aside to preserve it, or remove it, then restart 9a", err, path), closeErr)
		}
		return nil, errors.Join(err, closeErr)
	}
	if err = secureSQLiteFiles(path, false); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

func secureSQLiteFiles(path string, createMain bool) error {
	if err := secureSQLiteFile(path, createMain); err != nil {
		return err
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := secureSQLiteFile(path+suffix, false); err != nil {
			return err
		}
	}
	return nil
}

func secureSQLiteFile(path string, create bool) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) && !create {
		return nil
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect sqlite file %q: %w", path, err)
	}
	if err == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return fmt.Errorf("sqlite file %q must be a regular file, not a link", path)
	}
	flags := os.O_RDWR
	if create {
		flags |= os.O_CREATE
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if errors.Is(err, os.ErrNotExist) && !create {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open sqlite file %q: %w", path, err)
	}
	openedInfo, statErr := file.Stat()
	if statErr != nil {
		_ = file.Close()
		return fmt.Errorf("inspect open sqlite file %q: %w", path, statErr)
	}
	if !openedInfo.Mode().IsRegular() {
		_ = file.Close()
		return fmt.Errorf("sqlite file %q must be a regular file", path)
	}
	if chmodErr := file.Chmod(0o600); chmodErr != nil {
		_ = file.Close()
		return fmt.Errorf("secure sqlite file %q: %w", path, chmodErr)
	}
	if closeErr := file.Close(); closeErr != nil {
		return fmt.Errorf("close sqlite file %q: %w", path, closeErr)
	}
	return nil
}

func prepareSchema(ctx context.Context, db *sql.DB) error {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return fmt.Errorf("read sqlite schema version: %w", err)
	}
	if version == schemaVersion {
		return nil
	}
	if version != 0 {
		return fmt.Errorf("%w: schema version %d, expected %d", ErrIncompatibleState, version, schemaVersion)
	}

	var objects int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*)
FROM sqlite_schema
WHERE type IN ('table', 'index', 'view', 'trigger') AND name NOT LIKE 'sqlite_%'
`).Scan(&objects); err != nil {
		return fmt.Errorf("inspect sqlite schema: %w", err)
	}
	if objects != 0 {
		return fmt.Errorf("%w: schema version %d, expected %d", ErrIncompatibleState, version, schemaVersion)
	}
	return initializeSchema(ctx, db)
}
