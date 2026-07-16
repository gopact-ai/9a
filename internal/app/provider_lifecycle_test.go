package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
)

type closeAdapter struct {
	mu     sync.Mutex
	closed []string
	err    error
}

func (a *closeAdapter) Discover(context.Context, provider.Provider) ([]capability.Capability, error) {
	return nil, nil
}

func (*closeAdapter) Invoke(context.Context, provider.Provider, capability.Capability, string, json.RawMessage, provider.Sink) error {
	return nil
}

func (*closeAdapter) Cancel(context.Context, provider.Provider, string) error { return nil }

func (*closeAdapter) Health(context.Context, provider.Provider) provider.Health {
	return provider.Health{Healthy: true}
}

func (a *closeAdapter) Close(_ context.Context, p provider.Provider) error {
	a.mu.Lock()
	a.closed = append(a.closed, p.Name)
	a.mu.Unlock()
	return a.err
}

func TestAppCloseAttemptsEveryProviderAndJoinsErrors(t *testing.T) {
	a, _ := testApp(t)
	firstErr := errors.New("first close failed")
	first := &closeAdapter{err: firstErr}
	second := &closeAdapter{}
	a.mu.Lock()
	a.adapters["first"] = first
	a.adapters["second"] = second
	a.providers["first/one"] = provider.Provider{ID: "first/one", Protocol: "first", Name: "one"}
	a.providers["second/two"] = provider.Provider{ID: "second/two", Protocol: "second", Name: "two"}
	a.mu.Unlock()
	closeCtx, cancel := context.WithCancel(context.Background())
	cancel()
	err := a.Close(closeCtx)
	if !errors.Is(err, firstErr) || !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() error=%v", err)
	}
	first.mu.Lock()
	firstClosed := append([]string(nil), first.closed...)
	first.mu.Unlock()
	second.mu.Lock()
	secondClosed := append([]string(nil), second.closed...)
	second.mu.Unlock()
	if len(firstClosed) != 1 || firstClosed[0] != "one" || len(secondClosed) != 1 || secondClosed[0] != "two" {
		t.Fatalf("closed first=%v second=%v", firstClosed, secondClosed)
	}
}
