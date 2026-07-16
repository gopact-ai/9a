package secret

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gopact-ai/9a/internal/workspace"
)

var (
	ErrMissing    = errors.New("secret is missing")
	referencePart = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

const MaxValueBytes = 2048

type MissingError struct {
	Reference string
}

func (e *MissingError) Error() string {
	return fmt.Sprintf("secret %q is missing", e.Reference)
}

func (*MissingError) Unwrap() error { return ErrMissing }

type Resolver interface {
	Resolve(context.Context, string) (string, error)
}

type workspaceContextKey struct{}

func WithWorkspace(ctx context.Context, root string) context.Context {
	if !filepath.IsAbs(root) {
		return ctx
	}
	return context.WithValue(ctx, workspaceContextKey{}, filepath.Clean(root))
}

// Backend implementations must not include secret values in returned errors.
type Backend interface {
	Set(context.Context, string, string) error
	Get(context.Context, string) (string, bool, error)
	Delete(context.Context, string) error
}

type Metadata struct {
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Service struct {
	db      *sql.DB
	backend Backend
	// ponytail: secret changes are rare; use one lock until measured contention justifies per-reference locks.
	mu sync.RWMutex
}

var _ Resolver = (*Service)(nil)

func NewService(db *sql.DB, backend Backend) *Service {
	return &Service{db: db, backend: backend}
}

func ValidateReference(reference string) error {
	parts := strings.Split(reference, ".")
	if len(parts) != 2 || !referencePart.MatchString(parts[0]) || !referencePart.MatchString(parts[1]) {
		return fmt.Errorf("secret reference %q must be <integration>.<key>", reference)
	}
	return nil
}

func storageReference(ctx context.Context, reference string) string {
	root, _ := ctx.Value(workspaceContextKey{}).(string)
	if root == "" {
		return reference
	}
	return workspace.StableID(root) + ":" + reference
}

func visibleReference(ctx context.Context, stored string) (string, bool) {
	root, _ := ctx.Value(workspaceContextKey{}).(string)
	if root == "" {
		return stored, !strings.Contains(stored, ":")
	}
	prefix := workspace.StableID(root) + ":"
	if !strings.HasPrefix(stored, prefix) {
		return "", false
	}
	return strings.TrimPrefix(stored, prefix), true
}

func (s *Service) Set(ctx context.Context, reference, value string) error {
	if err := ValidateReference(reference); err != nil {
		return err
	}
	if value == "" {
		return errors.New("secret value is empty")
	}
	if len(value) > MaxValueBytes {
		return fmt.Errorf("secret value exceeds %d bytes", MaxValueBytes)
	}
	if s.backend == nil {
		return errors.New("secret backend is unavailable")
	}
	if s.db == nil {
		return errors.New("secret metadata store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := storageReference(ctx, reference)
	previous, existed, err := s.backend.Get(ctx, stored)
	if err != nil {
		return fmt.Errorf("get secret %q before set: %w", reference, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin secret %q metadata update: %w", reference, err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO secrets(name,created_at,updated_at) VALUES(?,?,?) ON CONFLICT(name) DO UPDATE SET updated_at=excluded.updated_at`, stored, now, now); err != nil {
		return fmt.Errorf("record secret %q metadata: %w", reference, err)
	}
	if err := s.backend.Set(ctx, stored, value); err != nil {
		return fmt.Errorf("set secret %q: %w", reference, err)
	}
	if err := tx.Commit(); err != nil {
		commitErr := fmt.Errorf("commit secret %q metadata: %w", reference, err)
		compensationCtx := context.WithoutCancel(ctx)
		if existed {
			err = s.backend.Set(compensationCtx, stored, previous)
		} else {
			err = s.backend.Delete(compensationCtx, stored)
		}
		if err != nil {
			return errors.Join(commitErr, fmt.Errorf("restore secret %q after metadata failure: %w", reference, err))
		}
		return commitErr
	}
	return nil
}

func (s *Service) Resolve(ctx context.Context, reference string) (string, error) {
	if err := ValidateReference(reference); err != nil {
		return "", err
	}
	if s.backend == nil {
		return "", &MissingError{Reference: reference}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	value, ok, err := s.backend.Get(ctx, storageReference(ctx, reference))
	if err != nil {
		return "", fmt.Errorf("get secret %q: %w", reference, err)
	}
	if !ok || value == "" {
		return "", &MissingError{Reference: reference}
	}
	return value, nil
}

func (s *Service) Delete(ctx context.Context, reference string) error {
	if err := ValidateReference(reference); err != nil {
		return err
	}
	if s.backend == nil {
		return errors.New("secret backend is unavailable")
	}
	if s.db == nil {
		return errors.New("secret metadata store is unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	stored := storageReference(ctx, reference)
	previous, existed, err := s.backend.Get(ctx, stored)
	if err != nil {
		return fmt.Errorf("get secret %q before delete: %w", reference, err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin secret %q metadata deletion: %w", reference, err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM secrets WHERE name=?`, stored); err != nil {
		return fmt.Errorf("delete secret %q metadata: %w", reference, err)
	}
	if err := s.backend.Delete(ctx, stored); err != nil {
		return fmt.Errorf("delete secret %q: %w", reference, err)
	}
	if err := tx.Commit(); err != nil {
		commitErr := fmt.Errorf("commit secret %q metadata deletion: %w", reference, err)
		if existed {
			if err := s.backend.Set(context.WithoutCancel(ctx), stored, previous); err != nil {
				return errors.Join(commitErr, fmt.Errorf("restore secret %q after metadata failure: %w", reference, err))
			}
		}
		return commitErr
	}
	return nil
}

func (s *Service) Declared(ctx context.Context, reference string) (bool, error) {
	if err := ValidateReference(reference); err != nil {
		return false, err
	}
	if s.db == nil {
		return false, errors.New("secret metadata store is unavailable")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	var declared bool
	if err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM secrets WHERE name=?)`, storageReference(ctx, reference)).Scan(&declared); err != nil {
		return false, fmt.Errorf("check secret %q metadata: %w", reference, err)
	}
	return declared, nil
}

func (s *Service) List(ctx context.Context) ([]Metadata, error) {
	if s.db == nil {
		return nil, errors.New("secret metadata store is unavailable")
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	rows, err := s.db.QueryContext(ctx, `SELECT name,created_at,updated_at FROM secrets ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list secret metadata: %w", err)
	}
	metadata := make([]Metadata, 0)
	for rows.Next() {
		var item Metadata
		var createdAt, updatedAt string
		if err := rows.Scan(&item.Name, &createdAt, &updatedAt); err != nil {
			return nil, fmt.Errorf("scan secret metadata: %w", err)
		}
		visible, ok := visibleReference(ctx, item.Name)
		if !ok {
			continue
		}
		item.Name = visible
		item.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
		if err != nil {
			return nil, fmt.Errorf("parse secret %q created_at: %w", item.Name, err)
		}
		item.UpdatedAt, err = time.Parse(time.RFC3339Nano, updatedAt)
		if err != nil {
			return nil, fmt.Errorf("parse secret %q updated_at: %w", item.Name, err)
		}
		metadata = append(metadata, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, fmt.Errorf("iterate secret metadata: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("close secret metadata: %w", err)
	}
	return metadata, nil
}
