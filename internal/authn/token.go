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
