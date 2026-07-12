package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/store"
)

const appDeclarativeSource = `apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: weather
  description: Current weather.
services:
  forecast:
    baseURL: SERVER_URL
operations:
  current:
    service: forecast
    method: GET
    path: /weather
    request:
      query:
        city: "{{ input.city }}"
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body
`

type cancelingCatalog struct {
	catalogRepository
	entered chan struct{}
}

func (c *cancelingCatalog) ReplaceProviderCapabilities(ctx context.Context, _ provider.Provider, _ []capability.Capability) (int64, error) {
	close(c.entered)
	<-ctx.Done()
	return 0, ctx.Err()
}

func TestDeclarativeSourceLifecycleAndRestore(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"city": r.URL.Query().Get("city"), "temperature": 23})
	}))
	defer server.Close()
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "ninea.db")
	workspace := t.TempDir()
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", server.URL, 1))

	db, err := store.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	result, err := a.AddDeclarative(ctx, "admin", source, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "weather" || len(result.Capabilities) != 1 {
		t.Fatalf("result=%#v", result)
	}
	for _, path := range []string{"SKILL.md", "operations/current/invoke", "references/source.yaml"} {
		if _, err := os.Stat(filepath.Join(workspace, ".agents", "skills", "weather", path)); err != nil {
			t.Fatalf("projected %s: %v", path, err)
		}
	}
	output, err := a.Invoke(ctx, "admin", "api/weather/current", json.RawMessage(`{"city":"Shanghai"}`))
	if err != nil || string(output) != `{"city":"Shanghai","temperature":23}` {
		t.Fatalf("invoke=%s err=%v", output, err)
	}
	oversized := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, call.MaxPayloadBytes)...)
	oversized = append(oversized, '"')
	if _, err := a.Invoke(ctx, "admin", "api/weather/current", oversized); !errors.Is(err, call.ErrPayloadTooLarge) {
		t.Fatalf("oversized invoke error=%v", err)
	}
	diff, err := a.DiffDeclarative(ctx, source)
	if err != nil || diff.Changed {
		t.Fatalf("diff=%#v err=%v", diff, err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a = New(db)
	if err := a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	output, err = a.Invoke(ctx, "admin", "api/weather/current", json.RawMessage(`{"city":"Beijing"}`))
	if err != nil || string(output) != `{"city":"Beijing","temperature":23}` {
		t.Fatalf("restored invoke=%s err=%v", output, err)
	}
	if err := a.RemoveDeclarative(ctx, "admin", "weather"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agents", "skills", "weather")); !os.IsNotExist(err) {
		t.Fatalf("projection remains: %v", err)
	}
}

func TestDeclarativeUpdateAndRemoveRollbackOnDatabaseFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", server.URL, 1))
	if _, err := a.AddDeclarative(ctx, "admin", source, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_api_update BEFORE UPDATE ON providers WHEN NEW.protocol='api' BEGIN SELECT RAISE(FAIL,'reject'); END`); err != nil {
		t.Fatal(err)
	}
	updated := []byte(strings.Replace(string(source), "Current weather.", "Changed weather.", 1))
	if _, err := a.AddDeclarative(ctx, "admin", updated, workspace); err == nil {
		t.Fatal("update unexpectedly passed")
	}
	projected := filepath.Join(workspace, ".agents", "skills", "weather", "references", "source.yaml")
	data, err := os.ReadFile(projected)
	if err != nil || string(data) != string(source) {
		t.Fatalf("rollback source=%q err=%v", data, err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_api_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_api_delete BEFORE DELETE ON providers WHEN OLD.protocol='api' BEGIN SELECT RAISE(FAIL,'reject'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.RemoveDeclarative(ctx, "admin", "weather"); err == nil {
		t.Fatal("remove unexpectedly passed")
	}
	if _, err := os.Stat(projected); err != nil {
		t.Fatalf("remove rollback projection: %v", err)
	}
	output, err := a.Invoke(ctx, "admin", "api/weather/current", json.RawMessage(`{"city":"Shanghai"}`))
	if err != nil || string(output) != `{"ok":true}` {
		t.Fatalf("rollback invoke=%s err=%v", output, err)
	}
}

func TestConcurrentDeclarativeUpdatesStayConsistent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	base := strings.Replace(appDeclarativeSource, "SERVER_URL", server.URL, 1)
	sources := [][]byte{[]byte(strings.Replace(base, "Current weather.", "Weather version one.", 1)), []byte(strings.Replace(base, "Current weather.", "Weather version two.", 1))}
	var wait sync.WaitGroup
	errorsOut := make(chan error, len(sources))
	for _, source := range sources {
		source := source
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := a.AddDeclarative(ctx, "admin", source, workspace)
			errorsOut <- err
		}()
	}
	wait.Wait()
	close(errorsOut)
	for err := range errorsOut {
		if err != nil {
			t.Fatal(err)
		}
	}
	p, err := a.declarativeProvider(ctx, "api/weather")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("provider missing")
	}
	projected, err := os.ReadFile(filepath.Join(workspace, ".agents", "skills", "weather", "references", "source.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(projected) != p.Config["source"] {
		t.Fatalf("projection and provider diverged")
	}
}

func TestDeclarativeAddCancellationUsesDetachedRollback(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	gate := &cancelingCatalog{catalogRepository: a.cat, entered: make(chan struct{})}
	a.cat = gate
	workspace := t.TempDir()
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", "http://127.0.0.1:1", 1))
	requestCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := a.AddDeclarative(requestCtx, "admin", source, workspace); done <- err }()
	<-gate.entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	skill := filepath.Join(workspace, ".agents", "skills", "weather")
	if _, err := os.Stat(skill); !os.IsNotExist(err) {
		t.Fatalf("projection survived canceled add: %v", err)
	}
	var providers int
	if err := db.QueryRow(`SELECT count(*) FROM providers WHERE id='api/weather'`).Scan(&providers); err != nil {
		t.Fatal(err)
	}
	if providers != 0 {
		t.Fatalf("providers=%d", providers)
	}
}
