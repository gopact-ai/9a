package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	adapterreg "github.com/gopact-ai/9a/internal/adapter"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/store"
)

func testExecutable(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func testApp(t *testing.T) (*App, *adapterreg.Repository) {
	t.Helper()
	db, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return New(db), adapterreg.NewRepository(db)
}

func TestNewIncludesBuiltInA2AAdapter(t *testing.T) {
	a, _ := testApp(t)
	if a.adapters["a2a"] == nil {
		t.Fatal("built-in A2A adapter is missing")
	}
}

func TestAddAdapterConcurrentDuplicateKeepsDatabaseAndMapConsistent(t *testing.T) {
	ctx := context.Background()
	a, repo := testApp(t)
	executable := testExecutable(t, "billing-adapter")
	errs := make(chan error, 2)
	var start sync.WaitGroup
	start.Add(1)
	for range 2 {
		go func() {
			start.Wait()
			errs <- a.AddAdapter(ctx, "billing", executable)
		}()
	}
	start.Done()
	var successes, duplicates int
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			successes++
		case errors.Is(err, adapterreg.ErrDuplicate):
			duplicates++
		default:
			t.Fatalf("AddAdapter() error=%v", err)
		}
	}
	if successes != 1 || duplicates != 1 {
		t.Fatalf("successes=%d duplicates=%d", successes, duplicates)
	}
	registrations, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(registrations) != 1 || registrations[0].Protocol != "billing" {
		t.Fatalf("registrations=%#v", registrations)
	}
	a.mu.RLock()
	_, registered := a.adapters["billing"]
	a.mu.RUnlock()
	if !registered {
		t.Fatal("database registration was not installed in adapter map")
	}
}

func TestAddAdapterValidationAndDuplicateFailuresDoNotDivergeState(t *testing.T) {
	ctx := context.Background()
	a, repo := testApp(t)
	executable := testExecutable(t, "billing-adapter")
	if err := a.AddAdapter(ctx, "Billing API", executable); !errors.Is(err, adapterreg.ErrInvalid) {
		t.Fatalf("invalid AddAdapter() error=%v", err)
	}
	registrations, err := repo.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(registrations) != 0 {
		t.Fatalf("invalid registration persisted: %#v", registrations)
	}
	a.mu.RLock()
	_, invalidInstalled := a.adapters["Billing API"]
	a.mu.RUnlock()
	if invalidInstalled {
		t.Fatal("invalid registration installed in memory")
	}
	if err := a.AddAdapter(ctx, "billing", executable); err != nil {
		t.Fatal(err)
	}
	other := testExecutable(t, "other-adapter")
	if err := a.AddAdapter(ctx, "billing", other); !errors.Is(err, adapterreg.ErrDuplicate) {
		t.Fatalf("duplicate AddAdapter() error=%v", err)
	}
	registration, err := repo.Get(ctx, "billing")
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	if registration.Executable != want {
		t.Fatalf("duplicate replaced persisted executable: %q", registration.Executable)
	}
}

func TestRestoreRejectsMissingAdapterExecutable(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	executable := testExecutable(t, "temporary-adapter")
	if err := a.AddAdapter(ctx, "temporary", executable); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(executable); err != nil {
		t.Fatal(err)
	}
	restored := New(a.db)
	err := restored.Restore(ctx)
	if !errors.Is(err, adapterreg.ErrInvalid) || !strings.Contains(err.Error(), "temporary") {
		t.Fatalf("Restore() error=%v", err)
	}
}

func TestRestoreRejectsReservedAndNoncanonicalRegistryCorruption(t *testing.T) {
	for _, protocol := range []string{"mcp", "Billing"} {
		t.Run(protocol, func(t *testing.T) {
			ctx := context.Background()
			a, _ := testApp(t)
			executable := testExecutable(t, "adapter")
			if _, err := a.db.ExecContext(ctx, `INSERT INTO external_adapters(protocol,executable,created_at) VALUES(?,?,?)`, protocol, executable, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				t.Fatal(err)
			}
			err := a.Restore(ctx)
			if !errors.Is(err, adapterreg.ErrInvalid) || !strings.Contains(err.Error(), protocol) {
				t.Fatalf("Restore() error=%v", err)
			}
		})
	}
}

func TestRestoreLoadsExternalAdaptersBeforeProviders(t *testing.T) {
	ctx := context.Background()
	a, repo := testApp(t)
	executable := testExecutable(t, "billing-adapter")
	canonical, err := adapterreg.ValidateRegistration("billing", executable)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Add(ctx, "billing", canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO providers(id,protocol,name,endpoint,revision,config_json) VALUES('billing/invoices','billing','invoices','local',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	if err := a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	_, adapterLoaded := a.adapters["billing"]
	providerLoaded := a.providers["billing/invoices"].Protocol == "billing"
	a.mu.RUnlock()
	if !adapterLoaded || !providerLoaded {
		t.Fatalf("adapterLoaded=%v providerLoaded=%v", adapterLoaded, providerLoaded)
	}
}

func TestRestoreStillRejectsProviderWithoutAdapterRegistration(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	if _, err := a.db.ExecContext(ctx, `INSERT INTO providers(id,protocol,name,endpoint,revision,config_json) VALUES('unknown/demo','unknown','demo','local',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	err := a.Restore(ctx)
	if err == nil || !strings.Contains(err.Error(), "unsupported protocol: unknown") {
		t.Fatalf("Restore() error=%v", err)
	}
}

func TestRestoreRejectsProviderWhoseIDDisagreesWithProtocolAndName(t *testing.T) {
	ctx := context.Background()
	a, repo := testApp(t)
	executable := testExecutable(t, "billing-adapter")
	canonical, err := adapterreg.ValidateRegistration("billing", executable)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.Add(ctx, "billing", canonical); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `INSERT INTO providers(id,protocol,name,endpoint,revision,config_json) VALUES('wrong/invoices','billing','invoices','local',1,'{}')`); err != nil {
		t.Fatal(err)
	}
	err = a.Restore(ctx)
	if err == nil || !strings.Contains(err.Error(), "inconsistent provider id") || !strings.Contains(err.Error(), "wrong/invoices") {
		t.Fatalf("Restore() error=%v", err)
	}
}

func TestAddProviderRejectsIDThatDisagreesWithProtocolAndName(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	ad := &closeAdapter{}
	a.mu.Lock()
	a.adapters["billing"] = ad
	a.mu.Unlock()
	p := provider.Provider{ID: "wrong/invoices", Protocol: "billing", Name: "invoices", Endpoint: "local"}
	err := a.AddProvider(ctx, p)
	if err == nil || !strings.Contains(err.Error(), "inconsistent provider id") {
		t.Fatalf("AddProvider() error=%v", err)
	}
	var count int
	if err := a.db.QueryRowContext(ctx, `SELECT count(*) FROM providers WHERE id=?`, p.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("inconsistent provider was persisted")
	}
}

func TestRestoreRejectsAppWithInMemoryExternalAdapterWithoutReplacingMap(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	executable := testExecutable(t, "billing-adapter")
	if err := a.AddAdapter(ctx, "billing", executable); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	before := a.adapters["billing"]
	a.mu.RUnlock()
	err := a.Restore(ctx)
	if !errors.Is(err, ErrRestoreRequiresFreshApp) {
		t.Fatalf("Restore() error=%v", err)
	}
	a.mu.RLock()
	after := a.adapters["billing"]
	a.mu.RUnlock()
	if after != before {
		t.Fatal("rejected Restore replaced external adapter map entry")
	}
}
