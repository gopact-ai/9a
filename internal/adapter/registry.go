package adapter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/gopact-ai/9a/internal/capability"
	"golang.org/x/sys/unix"
)

var (
	ErrDuplicate = errors.New("adapter already registered")
	ErrNotFound  = errors.New("adapter registration not found")
	ErrInvalid   = errors.New("invalid adapter registration")
)

type Registration struct {
	Protocol   string
	Executable string
	CreatedAt  time.Time
}

type Repository struct{ db *sql.DB }

func NewRepository(db *sql.DB) *Repository { return &Repository{db: db} }

func (r *Repository) Add(ctx context.Context, protocol, executable string) (Registration, error) {
	created := time.Now().UTC()
	result, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO external_adapters(protocol,executable,created_at) VALUES(?,?,?)`, protocol, executable, created.Format(time.RFC3339Nano))
	if err != nil {
		return Registration{}, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return Registration{}, err
	}
	if rows == 0 {
		return Registration{}, ErrDuplicate
	}
	return Registration{Protocol: protocol, Executable: executable, CreatedAt: created}, nil
}

func (r *Repository) Get(ctx context.Context, protocol string) (Registration, error) {
	var registration Registration
	var created string
	err := r.db.QueryRowContext(ctx, `SELECT protocol,executable,created_at FROM external_adapters WHERE protocol=?`, protocol).Scan(&registration.Protocol, &registration.Executable, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Registration{}, ErrNotFound
	}
	if err != nil {
		return Registration{}, err
	}
	registration.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	if err != nil {
		return Registration{}, fmt.Errorf("invalid adapter created_at: %w", err)
	}
	return registration, nil
}

func (r *Repository) List(ctx context.Context) ([]Registration, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT protocol,executable,created_at FROM external_adapters ORDER BY protocol`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var registrations []Registration
	for rows.Next() {
		var registration Registration
		var created string
		if err := rows.Scan(&registration.Protocol, &registration.Executable, &created); err != nil {
			return nil, err
		}
		registration.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
		if err != nil {
			return nil, fmt.Errorf("invalid adapter created_at: %w", err)
		}
		registrations = append(registrations, registration)
	}
	return registrations, rows.Err()
}

func IsReservedProtocol(protocol string) bool {
	switch protocol {
	case "mcp", "a2a":
		return true
	default:
		return false
	}
}

func ValidateRegistration(protocol, executable string) (string, error) {
	if protocol == "" || capability.Slug(protocol) != protocol {
		return "", fmt.Errorf("%w: protocol must be a canonical non-empty slug", ErrInvalid)
	}
	if IsReservedProtocol(protocol) {
		return "", fmt.Errorf("%w: protocol is reserved", ErrInvalid)
	}
	return ValidateExecutable(executable)
}

func ValidateExecutable(executable string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("%w: executable path must be absolute", ErrInvalid)
	}
	canonical, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("%w: executable path cannot be resolved", ErrInvalid)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", fmt.Errorf("%w: executable path is unavailable", ErrInvalid)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("%w: executable path is not a regular file", ErrInvalid)
	}
	if info.Mode().Perm()&0111 == 0 || unix.Access(canonical, unix.X_OK) != nil {
		return "", fmt.Errorf("%w: executable path is not executable", ErrInvalid)
	}
	return filepath.Clean(canonical), nil
}
