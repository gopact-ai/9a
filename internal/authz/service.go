package authz

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type Permission string

const (
	Read   Permission = "read"
	Invoke Permission = "invoke"
	Admin  Permission = "admin"
)

func validateGrant(identity, capability string, permission Permission) error {
	if strings.TrimSpace(identity) == "" {
		return fmt.Errorf("identity must be non-empty")
	}
	if strings.TrimSpace(capability) == "" {
		return fmt.Errorf("capability must be non-empty")
	}
	switch permission {
	case Read, Invoke, Admin:
	default:
		return fmt.Errorf("invalid permission %q: expected read, invoke, or admin", permission)
	}
	return nil
}

type Service struct{ db *sql.DB }

func New(db *sql.DB) *Service { return &Service{db: db} }

func (s *Service) GrantIfAbsent(ctx context.Context, identity, capability string, permission Permission) (bool, error) {
	if err := validateGrant(identity, capability, permission); err != nil {
		return false, err
	}
	result, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO acl(identity_id,capability_id,permission) VALUES(?,?,?)`, identity, capability, string(permission))
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}
func (s *Service) Revoke(ctx context.Context, identity, capability string, permission Permission) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM acl WHERE identity_id=? AND capability_id=? AND permission=?`, identity, capability, string(permission))
	return err
}
func (s *Service) Allowed(ctx context.Context, identity, capability string, permission Permission) bool {
	var one int
	return s.db.QueryRowContext(ctx, `SELECT 1 FROM acl WHERE identity_id=? AND capability_id=? AND permission=?`, identity, capability, string(permission)).Scan(&one) == nil
}
