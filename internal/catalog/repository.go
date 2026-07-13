package catalog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
)

var ErrNotFound = errors.New("capability not found")

type Repository struct{ db *sql.DB }

func New(db *sql.DB) *Repository { return &Repository{db: db} }

func (r *Repository) Revision(ctx context.Context) (int64, error) {
	var rev int64
	err := r.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='catalog_revision'`).Scan(&rev)
	return rev, err
}

func (r *Repository) ReplaceProviderCapabilities(ctx context.Context, p provider.Provider, caps []capability.Capability) (rev int64, err error) {
	for i := range caps {
		if err := caps[i].Validate(); err != nil {
			return 0, fmt.Errorf("validate %d: %w", i, err)
		}
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = tx.QueryRowContext(ctx, `UPDATE metadata SET value=value+1 WHERE key='catalog_revision' RETURNING value`).Scan(&rev); err != nil {
		return 0, err
	}
	config, _ := json.Marshal(p.Config)
	if _, err = tx.ExecContext(ctx, `INSERT INTO providers(id,protocol,name,endpoint,revision,config_json) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET protocol=excluded.protocol,name=excluded.name,endpoint=excluded.endpoint,revision=excluded.revision,config_json=excluded.config_json`, p.ID, p.Protocol, p.Name, p.Endpoint, rev, config); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM capability_fts WHERE id IN (SELECT id FROM capabilities WHERE provider_id=?)`, p.ID); err != nil {
		return 0, err
	}
	nextIDs := make(map[string]struct{}, len(caps))
	for _, c := range caps {
		nextIDs[c.ID] = struct{}{}
	}
	rows, queryErr := tx.QueryContext(ctx, `SELECT id FROM capabilities WHERE provider_id=?`, p.ID)
	if queryErr != nil {
		return 0, queryErr
	}
	var removedIDs []string
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			_ = rows.Close()
			return 0, scanErr
		}
		if _, retained := nextIDs[id]; !retained {
			removedIDs = append(removedIDs, id)
		}
	}
	if rowsErr := rows.Err(); rowsErr != nil {
		_ = rows.Close()
		return 0, rowsErr
	}
	if closeErr := rows.Close(); closeErr != nil {
		return 0, closeErr
	}
	for _, id := range removedIDs {
		if _, err = tx.ExecContext(ctx, `DELETE FROM acl WHERE capability_id=?`, id); err != nil {
			return 0, err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM capabilities WHERE provider_id=?`, p.ID); err != nil {
		return 0, err
	}
	for _, c := range caps {
		c.Revision = rev
		data, marshalErr := json.Marshal(c)
		if marshalErr != nil {
			err = marshalErr
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO capabilities(id,provider_id,revision,kind,name,description,protocol,provider_name,data_json) VALUES(?,?,?,?,?,?,?,?,?)`, c.ID, p.ID, rev, c.Kind, c.Name, c.Description, c.Source.Protocol, c.Source.Provider, data); err != nil {
			return 0, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO capability_fts(id,name,description,tags,examples) VALUES(?,?,?,?,?)`, c.ID, c.Name, c.Description, strings.Join(c.Tags, " "), strings.Join(c.Examples, " ")); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return rev, nil
}

func (r *Repository) GetCapability(ctx context.Context, id string) (capability.Capability, error) {
	var data []byte
	if err := r.db.QueryRowContext(ctx, `SELECT data_json FROM capabilities WHERE id=?`, id).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return capability.Capability{}, ErrNotFound
		}
		return capability.Capability{}, err
	}
	var c capability.Capability
	if err := json.Unmarshal(data, &c); err != nil {
		return c, err
	}
	return c, nil
}

func (r *Repository) ListCapabilities(ctx context.Context) ([]capability.Capability, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT data_json FROM capabilities ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []capability.Capability
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var c capability.Capability
		if err := json.Unmarshal(data, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *Repository) DB() *sql.DB { return r.db }

func (r *Repository) ListProviders(ctx context.Context) ([]provider.Provider, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,protocol,name,endpoint,config_json FROM providers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []provider.Provider
	for rows.Next() {
		var p provider.Provider
		var raw []byte
		if err := rows.Scan(&p.ID, &p.Protocol, &p.Name, &p.Endpoint, &raw); err != nil {
			return nil, err
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &p.Config); err != nil {
				return nil, err
			}
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (r *Repository) DeleteProvider(ctx context.Context, providerID string) (err error) {
	return r.deleteProvider(ctx, providerID, true)
}

func (r *Repository) DeleteProviderPreservingACL(ctx context.Context, providerID string) (err error) {
	return r.deleteProvider(ctx, providerID, false)
}

func (r *Repository) deleteProvider(ctx context.Context, providerID string, deleteACL bool) (err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if deleteACL {
		if _, err = tx.ExecContext(ctx, `DELETE FROM acl WHERE capability_id IN (SELECT id FROM capabilities WHERE provider_id=?)`, providerID); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM capability_fts WHERE id IN (SELECT id FROM capabilities WHERE provider_id=?)`, providerID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM providers WHERE id=?`, providerID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE metadata SET value=value+1 WHERE key='catalog_revision'`); err != nil {
		return err
	}
	return tx.Commit()
}
