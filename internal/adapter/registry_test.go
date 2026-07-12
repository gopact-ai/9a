package adapter

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gopact-ai/9a/internal/store"
)

func TestRepositoryAddGetListRejectsDuplicatesAndPersists(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ninea.db")
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	repo := NewRepository(db)
	first, err := repo.Add(ctx, "zeta", "/opt/zeta")
	if err != nil {
		t.Fatal(err)
	}
	if first.Protocol != "zeta" || first.Executable != "/opt/zeta" || first.CreatedAt.IsZero() {
		t.Fatalf("Add()=%#v", first)
	}
	if _, err := repo.Add(ctx, "zeta", "/opt/other"); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate Add() error=%v", err)
	}
	if _, err := repo.Add(ctx, "alpha", "/opt/alpha"); err != nil {
		t.Fatal(err)
	}
	got, err := repo.Get(ctx, "zeta")
	if err != nil || got != first {
		t.Fatalf("Get()=%#v, %v", got, err)
	}
	list, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	protocols := []string{list[0].Protocol, list[1].Protocol}
	if !reflect.DeepEqual(protocols, []string{"alpha", "zeta"}) {
		t.Fatalf("List() order=%v", protocols)
	}
	if _, err := repo.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Get() error=%v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	got, err = NewRepository(db).Get(ctx, "zeta")
	if err != nil || got != first {
		t.Fatalf("Get() after reopen=%#v, %v", got, err)
	}
}

func TestExternalAdapterTableStoresOnlyRegistrationFields(t *testing.T) {
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	rows, err := db.Query(`PRAGMA table_info(external_adapters)`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if !reflect.DeepEqual(columns, []string{"protocol", "executable", "created_at"}) {
		t.Fatalf("columns=%v", columns)
	}
}

func TestValidateRegistrationCanonicalizesExecutableAndRejectsUnsafeInputs(t *testing.T) {
	dir := t.TempDir()
	realPath := filepath.Join(dir, "adapter-real")
	if err := os.WriteFile(realPath, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(dir, "adapter-link")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatal(err)
	}
	canonical, err := ValidateRegistration("billing-api", linkPath)
	if err != nil {
		t.Fatal(err)
	}
	wantCanonical, err := filepath.EvalSymlinks(realPath)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != wantCanonical {
		t.Fatalf("canonical path=%q want %q", canonical, wantCanonical)
	}

	nonExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("data"), 0600); err != nil {
		t.Fatal(err)
	}
	for name, input := range map[string]struct {
		protocol string
		path     string
	}{
		"empty protocol":      {path: realPath},
		"noncanonical slug":   {protocol: "Billing API", path: realPath},
		"reserved protocol":   {protocol: "mcp", path: realPath},
		"relative path":       {protocol: "billing", path: "adapter"},
		"missing path":        {protocol: "billing", path: filepath.Join(dir, "missing")},
		"directory":           {protocol: "billing", path: dir},
		"not executable mode": {protocol: "billing", path: nonExecutable},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateRegistration(input.protocol, input.path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("ValidateRegistration() error=%v", err)
			}
		})
	}
	if !IsReservedProtocol("mcp") || !IsReservedProtocol("a2a") || IsReservedProtocol("billing") {
		t.Fatalf("reserved protocol check returned unexpected values")
	}
}
