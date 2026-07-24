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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/store"
)

const appDeclarativeSource = `version: 1
name: weather
description: Current weather.
type: http
services:
  forecast:
    baseURL: SERVER_URL
capabilities:
  current:
    service: forecast
    method: GET
    path: /weather
    request:
      query:
        city: "{{ input.city }}"
    inputSchema:
      type: object
      required: [city]
      properties:
        city: {type: string}
    outputSchema:
      type: object
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

func TestConnectRejectsSymlinkedIntegrationDirectory(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(workspace, ".9a")); err != nil {
		t.Fatal(err)
	}
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", "https://example.com", 1))
	if _, err := a.Connect(ctx, "admin", source, workspace); err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("Connect error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "integrations", "weather.yaml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside workspace was modified: %v", err)
	}
}

func TestConnectDoesNotSnapshotOrRollbackSymlinkedCanonicalSource(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	integrations := filepath.Join(workspace, ".9a", "integrations")
	if err := os.MkdirAll(integrations, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "private-value")
	const privateValue = "must-not-be-copied-into-the-workspace"
	if err := os.WriteFile(outside, []byte(privateValue), 0o600); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(integrations, "weather.yaml")
	if err := os.Symlink(outside, canonical); err != nil {
		t.Fatal(err)
	}
	// This unowned projection would force a rollback after the canonical source
	// write in the vulnerable implementation.
	if err := os.MkdirAll(filepath.Join(workspace, ".agents", "skills", "using-ninea"), 0o755); err != nil {
		t.Fatal(err)
	}
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", "https://example.com", 1))
	if _, err := a.Connect(ctx, "admin", source, workspace); err == nil {
		t.Fatal("Connect accepted a symlinked canonical source")
	}
	info, err := os.Lstat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		data, readErr := os.ReadFile(canonical)
		t.Fatalf("canonical source symlink was replaced; data=%q error=%v", data, readErr)
	}
	data, err := os.ReadFile(outside)
	if err != nil || string(data) != privateValue {
		t.Fatalf("outside source changed: data=%q error=%v", data, err)
	}
}

func TestConnectRejectsOversizedPreviousCanonicalSource(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	integrations := filepath.Join(workspace, ".9a", "integrations")
	if err := os.MkdirAll(integrations, 0o755); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(integrations, "weather.yaml")
	previous := bytes.Repeat([]byte{'x'}, declarative.MaxSourceBytes+1)
	if err := os.WriteFile(canonical, previous, 0o644); err != nil {
		t.Fatal(err)
	}
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", "https://example.com", 1))
	if _, err := a.Connect(ctx, "admin", source, workspace); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Connect error=%v", err)
	}
	info, err := os.Stat(canonical)
	if err != nil || info.Size() != int64(len(previous)) {
		t.Fatalf("previous canonical source changed: info=%v error=%v", info, err)
	}
}

func TestRestoreKeepsInvalidCanonicalSourceVisibleAsBroken(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "ninea.db")
	workspace := t.TempDir()
	cleanupReadOnlyProjection(t, workspace)
	db, err := store.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", "https://example.com", 1))
	if _, err := a.Connect(ctx, "admin", source, workspace); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	canonical := filepath.Join(workspace, ".9a", "integrations", "weather.yaml")
	if err := os.WriteFile(canonical, []byte("not: [valid"), 0o644); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a = New(db)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.Restore(ctx); err != nil {
		t.Fatalf("Restore error=%v", err)
	}
	status, err := a.Status(ctx, workspace, "weather")
	if err != nil || status.State != "broken" || len(status.Integrations) != 1 || status.Integrations[0].Capabilities != 0 {
		t.Fatalf("Status=%#v error=%v", status, err)
	}
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
	cleanupReadOnlyProjection(t, workspace)
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", server.URL, 1))

	db, err := store.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	result, err := a.Connect(ctx, "admin", source, workspace)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "weather" || result.Source != ".9a/integrations/weather.yaml" || len(result.Capabilities) != 1 {
		t.Fatalf("result=%#v", result)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agents", "skills", "using-ninea", "SKILL.md")); err != nil {
		t.Fatalf("gateway skill: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agents", "skills", "weather")); !os.IsNotExist(err) {
		t.Fatalf("connect projected integration-specific skill: %v", err)
	}
	canonical := filepath.Join(workspace, ".9a", "integrations", "weather.yaml")
	data, err := os.ReadFile(canonical)
	if err != nil || string(data) != string(source) {
		t.Fatalf("canonical source=%q err=%v", data, err)
	}
	info, err := os.Stat(canonical)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("canonical mode=%v", info.Mode().Perm())
	}
	p, err := a.integrationByName(ctx, workspace, "weather")
	if err != nil {
		t.Fatal(err)
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	wantConfig := map[string]string{"workspace_root": canonicalWorkspace}
	if p == nil || !reflect.DeepEqual(p.Config, wantConfig) {
		t.Fatalf("provider config=%#v", p)
	}
	output, err := a.RunInWorkspace(ctx, "admin", workspace, "weather/current", json.RawMessage(`{"city":"Shanghai"}`), "")
	if err != nil || string(output) != `{"city":"Shanghai","temperature":23}` {
		t.Fatalf("invoke=%s err=%v", output, err)
	}
	oversized := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, call.MaxPayloadBytes)...)
	oversized = append(oversized, '"')
	if _, err := a.RunInWorkspace(ctx, "admin", workspace, "weather/current", oversized, ""); !errors.Is(err, call.ErrPayloadTooLarge) {
		t.Fatalf("oversized invoke error=%v", err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restoredSource := []byte(strings.Replace(string(source), "Current weather.", "Restored weather.", 1))
	restoredSource = []byte(strings.Replace(string(restoredSource), "  current:\n", "  restored:\n", 1))
	if err := os.WriteFile(canonical, restoredSource, 0o644); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	a = New(db)
	if err := a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	adapter := a.adapters["api"].(*declarative.Adapter)
	restoredProvider, err := a.integrationByName(ctx, workspace, "weather")
	if err != nil || restoredProvider == nil {
		t.Fatalf("restored provider=%#v err=%v", restoredProvider, err)
	}
	registered := adapter.Snapshot(restoredProvider.ID)
	if registered == nil || registered.Description != "Restored weather." || registered.Capabilities["restored"].Path != "/weather" {
		t.Fatalf("restored config=%#v", registered)
	}
	output, err = a.RunInWorkspace(ctx, "admin", workspace, "weather/restored", json.RawMessage(`{"city":"Beijing"}`), "")
	if err != nil || string(output) != `{"city":"Beijing","temperature":23}` {
		t.Fatalf("restored run=%s err=%v", output, err)
	}
	if _, err := catalog.New(db).ResolveWorkspaceCapability(ctx, workspace, "weather/current"); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("removed capability resolve error=%v", err)
	}
	if err := a.DisconnectFromWorkspace(ctx, "admin", workspace, "weather"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agents", "skills", "using-ninea", "SKILL.md")); err != nil {
		t.Fatalf("disconnect removed gateway skill: %v", err)
	}
	data, err = os.ReadFile(canonical)
	if err != nil || string(data) != string(restoredSource) {
		t.Fatalf("remove changed canonical source=%q err=%v", data, err)
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
	defer func() { _ = db.Close() }()
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	cleanupReadOnlyProjection(t, workspace)
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", server.URL, 1))
	if _, err := a.Connect(ctx, "admin", source, workspace); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_api_update BEFORE UPDATE ON providers WHEN NEW.protocol='api' BEGIN SELECT RAISE(FAIL,'reject'); END`); err != nil {
		t.Fatal(err)
	}
	updated := []byte(strings.Replace(string(source), "Current weather.", "Changed weather.", 1))
	if _, err := a.Connect(ctx, "admin", updated, workspace); err == nil {
		t.Fatal("update unexpectedly passed")
	}
	canonical := filepath.Join(workspace, ".9a", "integrations", "weather.yaml")
	data, err := os.ReadFile(canonical)
	if err != nil || string(data) != string(source) {
		t.Fatalf("rollback source=%q err=%v", data, err)
	}
	if _, err := db.Exec(`DROP TRIGGER reject_api_update`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TRIGGER reject_api_delete BEFORE DELETE ON providers WHEN OLD.protocol='api' BEGIN SELECT RAISE(FAIL,'reject'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.DisconnectFromWorkspace(ctx, "admin", workspace, "weather"); err == nil {
		t.Fatal("remove unexpectedly passed")
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agents", "skills", "using-ninea", "SKILL.md")); err != nil {
		t.Fatalf("remove failure changed gateway skill: %v", err)
	}
	data, err = os.ReadFile(canonical)
	if err != nil || string(data) != string(source) {
		t.Fatalf("remove changed canonical source=%q err=%v", data, err)
	}
	output, err := a.RunInWorkspace(ctx, "admin", workspace, "weather/current", json.RawMessage(`{"city":"Shanghai"}`), "")
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
	defer func() { _ = db.Close() }()
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	workspace := t.TempDir()
	cleanupReadOnlyProjection(t, workspace)
	base := strings.Replace(appDeclarativeSource, "SERVER_URL", server.URL, 1)
	sources := [][]byte{[]byte(strings.Replace(base, "Current weather.", "Weather version one.", 1)), []byte(strings.Replace(base, "Current weather.", "Weather version two.", 1))}
	var wait sync.WaitGroup
	errorsOut := make(chan error, len(sources))
	for _, source := range sources {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := a.Connect(ctx, "admin", source, workspace)
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
	p, err := a.integrationByName(ctx, workspace, "weather")
	if err != nil {
		t.Fatal(err)
	}
	if p == nil {
		t.Fatal("provider missing")
	}
	canonical, err := os.ReadFile(filepath.Join(workspace, ".9a", "integrations", "weather.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := declarative.Parse(canonical)
	if err != nil {
		t.Fatal(err)
	}
	registered := a.adapters["api"].(*declarative.Adapter).Snapshot(p.ID)
	if registered == nil || registered.Digest != parsed.Digest {
		t.Fatalf("canonical and adapter diverged")
	}
	canonicalWorkspace, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(p.Config, map[string]string{"workspace_root": canonicalWorkspace}) {
		t.Fatalf("provider config=%#v", p.Config)
	}
}

func cleanupReadOnlyProjection(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
}

func TestDeclarativeAddCancellationUsesDetachedRollback(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	gate := &cancelingCatalog{catalogRepository: a.cat, entered: make(chan struct{})}
	a.cat = gate
	workspace := t.TempDir()
	cleanupReadOnlyProjection(t, workspace)
	source := []byte(strings.Replace(appDeclarativeSource, "SERVER_URL", "http://127.0.0.1:1", 1))
	requestCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { _, err := a.Connect(requestCtx, "admin", source, workspace); done <- err }()
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
	if err := db.QueryRow(`SELECT count(*) FROM providers WHERE protocol='api' AND name='weather'`).Scan(&providers); err != nil {
		t.Fatal(err)
	}
	if providers != 0 {
		t.Fatalf("providers=%d", providers)
	}
	canonical := filepath.Join(workspace, ".9a", "integrations", "weather.yaml")
	if _, err := os.Stat(canonical); !os.IsNotExist(err) {
		t.Fatalf("canonical source survived canceled add: %v", err)
	}
}
