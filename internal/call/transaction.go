package call

import (
	"context"
	"database/sql"
	"errors"
)

type immediateTx struct {
	conn *sql.Conn
}

func beginImmediate(ctx context.Context, db *sql.DB) (*immediateTx, error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return &immediateTx{conn: conn}, nil
}

func (tx *immediateTx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return tx.conn.ExecContext(ctx, query, args...)
}

func (tx *immediateTx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return tx.conn.QueryRowContext(ctx, query, args...)
}

func (tx *immediateTx) Commit(ctx context.Context) error {
	_, commitErr := tx.conn.ExecContext(ctx, `COMMIT`)
	if commitErr != nil {
		_, _ = tx.conn.ExecContext(context.Background(), `ROLLBACK`)
	}
	return errors.Join(commitErr, tx.conn.Close())
}

func (tx *immediateTx) Rollback() error {
	_, rollbackErr := tx.conn.ExecContext(context.Background(), `ROLLBACK`)
	return errors.Join(rollbackErr, tx.conn.Close())
}
