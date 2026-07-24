// Package secret stores and resolves integration credentials, with a backend
// that keeps secret values in the operating system credential store via the
// system keyring.
package secret

import (
	"context"
	"errors"
	"fmt"

	keyring "github.com/zalando/go-keyring"
)

const keyringService = "dev.gopact.9a"

type KeyringBackend struct{}

func NewKeyringBackend() *KeyringBackend { return &KeyringBackend{} }

func (*KeyringBackend) Set(ctx context.Context, reference, value string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := keyring.Set(keyringService, reference, value); err != nil {
		return fmt.Errorf("system credential store write failed: %w", err)
	}
	return nil
}

func (*KeyringBackend) Get(ctx context.Context, reference string) (string, bool, error) {
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	value, err := keyring.Get(keyringService, reference)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("system credential store read failed: %w", err)
	}
	return value, true, nil
}

func (*KeyringBackend) Delete(ctx context.Context, reference string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := keyring.Delete(keyringService, reference); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("system credential store delete failed: %w", err)
	}
	return nil
}
