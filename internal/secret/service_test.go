package secret_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gopact-ai/9a/internal/secret"
	"github.com/gopact-ai/9a/internal/store"
)

type memoryBackend struct {
	mu     sync.Mutex
	values map[string]string
}

type blockingBackend struct {
	memoryBackend
	setStarted chan struct{}
	releaseSet chan struct{}
}

func (b *blockingBackend) Set(ctx context.Context, name, value string) error {
	close(b.setStarted)
	<-b.releaseSet
	return b.memoryBackend.Set(ctx, name, value)
}

func (b *memoryBackend) Set(_ context.Context, name, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.values == nil {
		b.values = make(map[string]string)
	}
	b.values[name] = value
	return nil
}

func (b *memoryBackend) Get(_ context.Context, name string) (string, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	value, ok := b.values[name]
	return value, ok, nil
}

func (b *memoryBackend) Delete(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.values, name)
	return nil
}

func TestServiceStoresOnlyMetadata(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	backend := &memoryBackend{}
	service := secret.NewService(db, backend)
	const reference = "weather.api-key"
	const stored = "stored-secret-canary"
	if err := service.Set(ctx, reference, stored); err != nil {
		t.Fatal(err)
	}

	declared, err := service.Declared(ctx, reference)
	if err != nil || !declared {
		t.Fatalf("declared=%v error=%v", declared, err)
	}
	metadata, err := service.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(metadata) != 1 || metadata[0].Name != reference || metadata[0].CreatedAt.IsZero() || metadata[0].UpdatedAt.IsZero() {
		t.Fatalf("metadata=%#v", metadata)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(secrets)`)
	if err != nil {
		t.Fatal(err)
	}
	var columns []string
	for rows.Next() {
		var position, notNull, primaryKey int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&position, &name, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(columns, ","); got != "name,created_at,updated_at" {
		t.Fatalf("secret metadata columns=%q", got)
	}

	var leaked int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM secrets WHERE name=? OR created_at LIKE ? OR updated_at LIKE ?`, stored, "%"+stored+"%", "%"+stored+"%").Scan(&leaked); err != nil {
		t.Fatal(err)
	}
	if leaked != 0 {
		t.Fatal("secret value was persisted in metadata")
	}

	value, err := service.Resolve(ctx, reference)
	if err != nil || value != stored {
		t.Fatalf("value=%q error=%v", value, err)
	}
	if err := service.Delete(ctx, reference); err != nil {
		t.Fatal(err)
	}
	declared, err = service.Declared(ctx, reference)
	if err != nil || declared {
		t.Fatalf("declared after delete=%v error=%v", declared, err)
	}
	_, err = service.Resolve(ctx, reference)
	if !errors.Is(err, secret.ErrMissing) || !strings.Contains(err.Error(), reference) || strings.Contains(err.Error(), stored) {
		t.Fatalf("missing error=%v", err)
	}
}

func TestServiceScopesSameReferenceByWorkspace(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	backend := &memoryBackend{}
	service := secret.NewService(db, backend)
	first := secret.WithWorkspace(ctx, "/work/first")
	second := secret.WithWorkspace(ctx, "/work/second")
	if err := service.Set(first, "weather.api-key", "first-value"); err != nil {
		t.Fatal(err)
	}
	if err := service.Set(second, "weather.api-key", "second-value"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		ctx  context.Context
		want string
	}{{first, "first-value"}, {second, "second-value"}} {
		got, err := service.Resolve(test.ctx, "weather.api-key")
		if err != nil || got != test.want {
			t.Fatalf("Resolve()=%q, %v; want %q", got, err, test.want)
		}
		metadata, err := service.List(test.ctx)
		if err != nil || len(metadata) != 1 || metadata[0].Name != "weather.api-key" {
			t.Fatalf("metadata=%#v err=%v", metadata, err)
		}
	}
}

func TestServiceRejectsOversizedValues(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	service := secret.NewService(db, &memoryBackend{})
	value := strings.Repeat("x", secret.MaxValueBytes+1)
	if err := service.Set(ctx, "weather.api-key", value); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("Set error=%v", err)
	}
}

func TestValidateReference(t *testing.T) {
	for _, reference := range []string{"weather", "weather.", ".api-key", "Weather.api-key", "weather.api.key"} {
		if err := secret.ValidateReference(reference); err == nil {
			t.Fatalf("ValidateReference(%q) succeeded", reference)
		}
	}
	if err := secret.ValidateReference("weather.api-key"); err != nil {
		t.Fatal(err)
	}
}

func TestSetDoesNotMutateBackendWhenMetadataStoreFails(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	backend := &memoryBackend{}
	service := secret.NewService(db, backend)
	if err := service.Set(ctx, "weather.api-key", "must-not-survive"); err == nil {
		t.Fatal("Set succeeded with a closed metadata store")
	}
	if value, found, err := backend.Get(ctx, "weather.api-key"); err != nil || found || value != "" {
		t.Fatalf("backend value=%q found=%v error=%v", value, found, err)
	}
}

func TestSetCompensatesBackendWhenMetadataCommitIsCanceled(t *testing.T) {
	db, err := store.Open(context.Background(), t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	backend := &blockingBackend{setStarted: make(chan struct{}), releaseSet: make(chan struct{})}
	service := secret.NewService(db, backend)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- service.Set(ctx, "weather.api-key", "must-be-compensated") }()
	<-backend.setStarted
	cancel()
	close(backend.releaseSet)
	if err := <-done; err == nil {
		t.Fatal("Set succeeded after its transaction was canceled")
	}
	if value, found, err := backend.Get(context.Background(), "weather.api-key"); err != nil || found || value != "" {
		t.Fatalf("backend value=%q found=%v error=%v", value, found, err)
	}
	declared, err := service.Declared(context.Background(), "weather.api-key")
	if err != nil || declared {
		t.Fatalf("declared=%v error=%v", declared, err)
	}
}

func TestConcurrentMutationsKeepBackendAndMetadataAligned(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	backend := &memoryBackend{}
	service := secret.NewService(db, backend)
	var group sync.WaitGroup
	for i := 0; i < 40; i++ {
		group.Add(1)
		go func(i int) {
			defer group.Done()
			if i%3 == 0 {
				if err := service.Delete(ctx, "weather.api-key"); err != nil {
					t.Errorf("Delete: %v", err)
				}
				return
			}
			if err := service.Set(ctx, "weather.api-key", fmt.Sprintf("value-%d", i)); err != nil {
				t.Errorf("Set: %v", err)
			}
		}(i)
	}
	group.Wait()
	declared, err := service.Declared(ctx, "weather.api-key")
	if err != nil {
		t.Fatal(err)
	}
	_, found, err := backend.Get(ctx, "weather.api-key")
	if err != nil {
		t.Fatal(err)
	}
	if declared != found {
		t.Fatalf("metadata declared=%v backend found=%v", declared, found)
	}
}
