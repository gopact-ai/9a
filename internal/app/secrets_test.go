package app

import (
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

	"github.com/gopact-ai/9a/internal/store"
)

type memorySecretBackend struct {
	mu     sync.Mutex
	values map[string]string
}

func (b *memorySecretBackend) Set(_ context.Context, name, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.values[name] = value
	return nil
}

func (b *memorySecretBackend) Get(_ context.Context, name string) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.values[name]
	return value, ok, nil
}

func (b *memorySecretBackend) Delete(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.values, name)
	return nil
}

func TestSecretsAreResolvedAtRunTimeWithoutRestart(t *testing.T) {
	const secretValue = "token-that-must-not-be-persisted"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secretValue {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	}))
	defer server.Close()
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	backend := &memorySecretBackend{values: map[string]string{}}
	a := NewWithSecretBackend(db, backend)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	source := []byte(`version: 1
name: private-api
type: http
credentials:
  api-token:
    secret: private-api.api-token
services:
  api:
    baseURL: ` + server.URL + `
    headers:
      Authorization: "Bearer {{ secrets.api-token }}"
capabilities:
  read:
    service: api
    method: GET
    path: /read
    inputSchema:
      type: object
      additionalProperties: false
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body
    outputSchema:
      type: object
      required: [ok]
      properties:
        ok: {type: boolean}
`)
	if _, err := a.Connect(ctx, "admin", source, root); err != nil {
		t.Fatal(err)
	}
	status, err := a.Status(ctx, root, "private-api")
	if err != nil || status.State != "needs-secret" || len(status.Integrations) != 1 || status.Integrations[0].State != "needs-secret" || len(status.Integrations[0].MissingSecrets) != 1 || status.Integrations[0].MissingSecrets[0] != "private-api.api-token" {
		t.Fatalf("status before set=%#v err=%v", status, err)
	}

	_, err = a.RunInWorkspace(ctx, "admin", root, "private-api/read", json.RawMessage(`{}`), "")
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Code != "missing_credential" || runErr.Credential != "private-api.api-token" || runErr.CallID == "" {
		t.Fatalf("missing run error=%#v", err)
	}
	if runErr.SideEffect != "none" {
		t.Fatalf("missing credential sideEffect=%q, want none", runErr.SideEffect)
	}
	statuses, err := a.ListSecrets(ctx, root, "private-api")
	if err != nil || len(statuses) != 1 || statuses[0] != (SecretStatus{Name: "private-api.api-token", State: "missing"}) {
		t.Fatalf("missing statuses=%#v err=%v", statuses, err)
	}

	if err := a.SetSecret(ctx, "admin", root, "private-api.api-token", secretValue); err != nil {
		t.Fatal(err)
	}
	status, err = a.Status(ctx, root, "")
	if err != nil || status.State != "ready" || len(status.Integrations) != 1 || status.Integrations[0].State != "ready" {
		t.Fatalf("status after set=%#v err=%v", status, err)
	}
	result, err := a.RunInWorkspace(ctx, "admin", root, "private-api/read", json.RawMessage(`{}`), "")
	if err != nil || string(result) != `{"ok":true}` {
		t.Fatalf("run after set=%s err=%v", result, err)
	}
	statuses, err = a.ListSecrets(ctx, root, "private-api")
	if err != nil || len(statuses) != 1 || statuses[0].State != "present" {
		t.Fatalf("present statuses=%#v err=%v", statuses, err)
	}
	var storedNames, leakedValues int
	if err := db.QueryRow(`SELECT count(*) FROM secrets WHERE name LIKE '%:private-api.api-token'`).Scan(&storedNames); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM secrets WHERE name=?`, secretValue).Scan(&leakedValues); err != nil {
		t.Fatal(err)
	}
	if storedNames != 1 || leakedValues != 0 {
		t.Fatalf("metadata names=%d leaked values=%d", storedNames, leakedValues)
	}

	if err := a.DeleteSecret(ctx, "admin", root, "private-api.api-token"); err != nil {
		t.Fatal(err)
	}
	statuses, err = a.ListSecrets(ctx, root, "private-api")
	if err != nil || len(statuses) != 1 || statuses[0].State != "missing" {
		t.Fatalf("unset statuses=%#v err=%v", statuses, err)
	}
	canonical := filepath.Join(root, ".9a", "integrations", "private-api.yaml")
	edited := []byte(strings.Replace(string(source), "name: private-api\n", "name: private-api\ndescription: edited offline\n", 1))
	if err := os.WriteFile(canonical, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	status, err = a.Status(ctx, root, "private-api")
	if err != nil || status.State != "broken" || status.Integrations[0].State != "broken" {
		t.Fatalf("status after source edit=%#v err=%v", status, err)
	}
}

func TestStatusReportsAnEmptyWorkspaceWithoutMutatingIt(t *testing.T) {
	a, _ := testApp(t)
	root := t.TempDir()
	status, err := a.Status(context.Background(), root, "")
	if err != nil || status.State != "empty" || len(status.Integrations) != 0 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".agents")); !os.IsNotExist(err) {
		t.Fatalf("status mutated the workspace: %v", err)
	}
}

func TestSecretMutationRequiresAdmin(t *testing.T) {
	a, _ := testApp(t)
	if err := a.SetSecret(context.Background(), "agent", t.TempDir(), "demo.token", "value"); err == nil {
		t.Fatal("non-admin set secret")
	}
	if err := a.DeleteSecret(context.Background(), "agent", t.TempDir(), "demo.token"); err == nil {
		t.Fatal("non-admin deleted secret")
	}
}
