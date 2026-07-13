package authn

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

var (
	ErrInvalidToken        = errors.New("invalid token")
	ErrAlreadyBootstrapped = errors.New("token store already bootstrapped")
)

type Service struct{ db *sql.DB }

func New(db *sql.DB) *Service { return &Service{db: db} }

func TokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func NewToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return "ninea_" + base64.RawURLEncoding.EncodeToString(buf), nil
}

func (s *Service) Create(ctx context.Context, identity string) (string, error) {
	token, err := NewToken()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO tokens(token_hash,identity_id,created_at) VALUES(?,?,?)`, TokenDigest(token), identity, time.Now().UTC().Format(time.RFC3339Nano))
	return token, err
}

func (s *Service) Import(ctx context.Context, token, identity string) error {
	if token == "" || identity == "" {
		return ErrInvalidToken
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var count int
	if err = tx.QueryRowContext(ctx, `SELECT count(*) FROM tokens`).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrAlreadyBootstrapped
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO tokens(token_hash,identity_id,created_at) VALUES(?,?,?)`, TokenDigest(token), identity, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) Authenticate(ctx context.Context, token string) (string, error) {
	var identity string
	err := s.db.QueryRowContext(ctx, `SELECT identity_id FROM tokens WHERE token_hash=?`, TokenDigest(token)).Scan(&identity)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrInvalidToken
	}
	return identity, err
}

func (s *Service) Count(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM tokens`).Scan(&n)
	return n, err
}
