package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/provider"
)

const lifecycleMCPHelperEnv = "NINEA_LIFECYCLE_MCP_HELPER"

func TestLifecycleMCPHelperProcess(t *testing.T) {
	if os.Getenv(lifecycleMCPHelperEnv) != "1" {
		return
	}
	type request struct {
		ID     int    `json:"id"`
		Method string `json:"method"`
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request request
		if json.Unmarshal(scanner.Bytes(), &request) != nil || request.ID == 0 {
			continue
		}
		var result any
		switch request.Method {
		case "initialize":
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{}, "serverInfo": map[string]any{"name": "lifecycle", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "route", "description": "Route request", "inputSchema": map[string]any{"type": "object"}}}}
		default:
			result = map[string]any{}
		}
		_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
	}
	os.Exit(0)
}

func lifecycleMCPExecutable(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mcp-helper")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestLifecycleMCPHelperProcess$\n", binary)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

type blockingCatalog struct {
	*catalog.Repository
	snapshot chan struct{}
	release  chan struct{}
}

func (c *blockingCatalog) ListProviders(ctx context.Context) ([]provider.Provider, error) {
	providers, err := c.Repository.ListProviders(ctx)
	close(c.snapshot)
	<-c.release
	return providers, err
}

func lifecycleCapability(protocol, name string) capability.Capability {
	return capability.Capability{
		ID: protocol + "/" + name + "/route", Kind: "api.operation", Name: "Route", Description: "Route request",
		Source: providerSource(protocol, name), Input: capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "json"},
	}
}

func providerSource(protocol, name string) capability.Source {
	return capability.Source{Protocol: protocol, Provider: name, UpstreamName: "route"}
}

type orderedAddAdapter struct {
	firstStarted, secondStarted chan struct{}
	releaseFirst, releaseSecond chan struct{}
}

func (a *orderedAddAdapter) Discover(_ context.Context, p provider.Provider) ([]capability.Capability, error) {
	switch p.Endpoint {
	case "first":
		close(a.firstStarted)
		<-a.releaseFirst
	case "second":
		close(a.secondStarted)
		<-a.releaseSecond
	}
	return []capability.Capability{lifecycleCapability(p.Protocol, p.Name)}, nil
}

func (*orderedAddAdapter) Invoke(_ context.Context, p provider.Provider, _ capability.Capability, _ string, _ json.RawMessage, sink provider.Sink) error {
	data, _ := json.Marshal(map[string]string{"endpoint": p.Endpoint})
	return sink.Event(provider.Event{Type: "result", Data: data})
}
func (*orderedAddAdapter) Cancel(context.Context, provider.Provider, string) error { return nil }
func (*orderedAddAdapter) Health(context.Context, provider.Provider) provider.Health {
	return provider.Health{Healthy: true}
}
func (*orderedAddAdapter) Close(context.Context, provider.Provider) error { return nil }

func TestConcurrentSameIDAddProviderSerializesCatalogMapAndInvokeRoute(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	ad := &orderedAddAdapter{
		firstStarted: make(chan struct{}), secondStarted: make(chan struct{}),
		releaseFirst: make(chan struct{}), releaseSecond: make(chan struct{}),
	}
	a.mu.Lock()
	a.adapters["ordered"] = ad
	a.mu.Unlock()
	first := provider.Provider{ID: "ordered/shared", Protocol: "ordered", Name: "shared", Endpoint: "first"}
	second := provider.Provider{ID: "ordered/shared", Protocol: "ordered", Name: "shared", Endpoint: "second"}
	firstErr := make(chan error, 1)
	secondErr := make(chan error, 1)
	go func() { firstErr <- a.AddProvider(ctx, first) }()
	<-ad.firstStarted
	go func() { secondErr <- a.AddProvider(ctx, second) }()
	select {
	case <-ad.secondStarted:
		close(ad.releaseSecond)
		if err := <-secondErr; err != nil {
			t.Fatal(err)
		}
		close(ad.releaseFirst)
		if err := <-firstErr; err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		close(ad.releaseFirst)
		if err := <-firstErr; err != nil {
			t.Fatal(err)
		}
		<-ad.secondStarted
		close(ad.releaseSecond)
		if err := <-secondErr; err != nil {
			t.Fatal(err)
		}
	}
	var catalogEndpoint string
	if err := a.db.QueryRowContext(ctx, `SELECT endpoint FROM providers WHERE id=?`, second.ID).Scan(&catalogEndpoint); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	mapEndpoint := a.providers[second.ID].Endpoint
	a.mu.RUnlock()
	if catalogEndpoint != "second" || mapEndpoint != "second" {
		t.Fatalf("catalog endpoint=%q map endpoint=%q", catalogEndpoint, mapEndpoint)
	}
	capabilityID := "ordered/shared/route"
	if err := a.Grant(ctx, "agent", capabilityID, []string{"invoke"}); err != nil {
		t.Fatal(err)
	}
	output, err := a.Invoke(ctx, "agent", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Endpoint string `json:"endpoint"`
	}
	if err := json.Unmarshal(output, &result); err != nil || result.Endpoint != "second" {
		t.Fatalf("Invoke()=%s, %v", output, err)
	}
}

type invokeUpdateAdapter struct {
	invokeStarted      chan struct{}
	releaseInvoke      chan struct{}
	badDiscoverStarted chan struct{}
	closeStarted       chan struct{}
	closed             chan struct{}
	closeOnce          sync.Once
}

func (a *invokeUpdateAdapter) Discover(_ context.Context, p provider.Provider) ([]capability.Capability, error) {
	if p.Endpoint == "bad" {
		close(a.badDiscoverStarted)
		return nil, errors.New("bad update")
	}
	return []capability.Capability{lifecycleCapability(p.Protocol, p.Name)}, nil
}

func (a *invokeUpdateAdapter) Invoke(_ context.Context, p provider.Provider, _ capability.Capability, _ string, _ json.RawMessage, sink provider.Sink) error {
	close(a.invokeStarted)
	select {
	case <-a.releaseInvoke:
		data, _ := json.Marshal(map[string]string{"endpoint": p.Endpoint})
		return sink.Event(provider.Event{Type: "result", Data: data})
	case <-a.closed:
		return errors.New("session closed during invoke")
	}
}
func (*invokeUpdateAdapter) Cancel(context.Context, provider.Provider, string) error { return nil }
func (*invokeUpdateAdapter) Health(context.Context, provider.Provider) provider.Health {
	return provider.Health{Healthy: true}
}
func (a *invokeUpdateAdapter) Close(context.Context, provider.Provider) error {
	a.closeOnce.Do(func() {
		close(a.closeStarted)
		close(a.closed)
	})
	return nil
}

func newInvokeUpdateApp(t *testing.T) (*App, *invokeUpdateAdapter, provider.Provider) {
	t.Helper()
	a, _ := testApp(t)
	ad := &invokeUpdateAdapter{
		invokeStarted: make(chan struct{}), releaseInvoke: make(chan struct{}), badDiscoverStarted: make(chan struct{}),
		closeStarted: make(chan struct{}), closed: make(chan struct{}),
	}
	a.mu.Lock()
	a.adapters["locked"] = ad
	a.mu.Unlock()
	p := provider.Provider{ID: "locked/shared", Protocol: "locked", Name: "shared", Endpoint: "good"}
	if err := a.AddProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(context.Background(), "agent", "locked/shared/route", []string{"invoke"}); err != nil {
		t.Fatal(err)
	}
	return a, ad, p
}

func TestFailedProviderUpdateCannotCloseConcurrentInvoke(t *testing.T) {
	ctx := context.Background()
	a, ad, good := newInvokeUpdateApp(t)
	invokeErr := make(chan error, 1)
	go func() {
		_, err := a.Invoke(ctx, "agent", "locked/shared/route", json.RawMessage(`{}`))
		invokeErr <- err
	}()
	<-ad.invokeStarted
	bad := good
	bad.Endpoint = "bad"
	updateErr := make(chan error, 1)
	go func() { updateErr <- a.AddProvider(ctx, bad) }()
	select {
	case <-ad.badDiscoverStarted:
		if err := <-updateErr; err == nil {
			t.Fatal("bad update succeeded")
		}
		if err := <-invokeErr; err != nil {
			t.Fatalf("Invoke failed during update: %v", err)
		}
	case <-time.After(100 * time.Millisecond):
		close(ad.releaseInvoke)
		if err := <-invokeErr; err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		<-ad.badDiscoverStarted
		if err := <-updateErr; err == nil {
			t.Fatal("bad update succeeded")
		}
	}
	a.mu.RLock()
	endpoint := a.providers[good.ID].Endpoint
	a.mu.RUnlock()
	if endpoint != "good" {
		t.Fatalf("provider endpoint=%q", endpoint)
	}
}

func TestCloseCancelsConcurrentInvokeBeforeWaitingForLease(t *testing.T) {
	ctx := context.Background()
	a, ad, _ := newInvokeUpdateApp(t)
	invokeErr := make(chan error, 1)
	go func() {
		_, err := a.Invoke(ctx, "agent", "locked/shared/route", json.RawMessage(`{}`))
		invokeErr <- err
	}()
	<-ad.invokeStarted
	closeErr := make(chan error, 1)
	go func() { closeErr <- a.Close(ctx) }()
	select {
	case <-ad.closeStarted:
	case <-time.After(time.Second):
		t.Fatal("Close did not initiate adapter termination")
	}
	if err := <-invokeErr; !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Invoke error=%v", err)
	}
	if err := <-closeErr; err != nil {
		t.Fatal(err)
	}
}

func TestRestoreSerializesWithAddProviderWithoutPublishingStaleSnapshot(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	a.mu.Lock()
	a.adapters["mcp"] = &orderedAddAdapter{}
	a.mu.Unlock()
	blocking := &blockingCatalog{Repository: a.cat.(*catalog.Repository), snapshot: make(chan struct{}), release: make(chan struct{})}
	a.cat = blocking
	t.Setenv(lifecycleMCPHelperEnv, "1")
	p := provider.Provider{ID: "mcp/shared", Protocol: "mcp", Name: "shared", Endpoint: "stdio:" + lifecycleMCPExecutable(t)}
	restoreErr := make(chan error, 1)
	go func() { restoreErr <- a.Restore(ctx) }()
	<-blocking.snapshot
	addErr := make(chan error, 1)
	go func() { addErr <- a.AddProvider(ctx, p) }()
	select {
	case err := <-addErr:
		if err != nil {
			t.Fatal(err)
		}
		close(blocking.release)
		if err := <-restoreErr; err != nil {
			t.Fatal(err)
		}
	case <-time.After(100 * time.Millisecond):
		close(blocking.release)
		if err := <-restoreErr; err != nil {
			t.Fatal(err)
		}
		if err := <-addErr; err != nil {
			t.Fatal(err)
		}
	}
	var catalogEndpoint string
	if err := a.db.QueryRowContext(ctx, `SELECT endpoint FROM providers WHERE id=?`, p.ID).Scan(&catalogEndpoint); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	mapEndpoint := a.providers[p.ID].Endpoint
	a.mu.RUnlock()
	if catalogEndpoint != p.Endpoint || mapEndpoint != p.Endpoint {
		t.Fatalf("catalog endpoint=%q map endpoint=%q", catalogEndpoint, mapEndpoint)
	}
}

type queuedOperationAdapter struct {
	mu            sync.Mutex
	discoverCalls int
	invokeCalls   int
	closeCalls    int
}

func (a *queuedOperationAdapter) Discover(_ context.Context, p provider.Provider) ([]capability.Capability, error) {
	a.mu.Lock()
	a.discoverCalls++
	a.mu.Unlock()
	return []capability.Capability{lifecycleCapability(p.Protocol, p.Name)}, nil
}
func (a *queuedOperationAdapter) Invoke(_ context.Context, p provider.Provider, _ capability.Capability, _ string, _ json.RawMessage, sink provider.Sink) error {
	a.mu.Lock()
	a.invokeCalls++
	a.mu.Unlock()
	data, _ := json.Marshal(map[string]string{"endpoint": p.Endpoint})
	return sink.Event(provider.Event{Type: "result", Data: data})
}
func (*queuedOperationAdapter) Cancel(context.Context, provider.Provider, string) error { return nil }
func (*queuedOperationAdapter) Health(context.Context, provider.Provider) provider.Health {
	return provider.Health{Healthy: true}
}
func (a *queuedOperationAdapter) Close(context.Context, provider.Provider) error {
	a.mu.Lock()
	a.closeCalls++
	a.mu.Unlock()
	return nil
}

func TestQueuedOperationsRecheckClosedStateAfterMutationGate(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	ad := &queuedOperationAdapter{}
	a.mu.Lock()
	a.adapters["queued"] = ad
	a.mu.Unlock()
	current := provider.Provider{ID: "queued/shared", Protocol: "queued", Name: "shared", Endpoint: "current"}
	if err := a.AddProvider(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(ctx, "agent", "queued/shared/route", []string{"invoke"}); err != nil {
		t.Fatal(err)
	}
	a.mutation.Lock()
	update := current
	update.Endpoint = "queued-update"
	addErr := make(chan error, 1)
	invokeErr := make(chan error, 1)
	go func() { addErr <- a.AddProvider(ctx, update) }()
	go func() {
		_, err := a.Invoke(ctx, "agent", "queued/shared/route", json.RawMessage(`{}`))
		invokeErr <- err
	}()
	deadline := time.Now().Add(time.Second)
	for {
		a.mu.Lock()
		active := len(a.leases)
		a.mu.Unlock()
		if active == 2 {
			break
		}
		if time.Now().After(deadline) {
			a.mutation.Unlock()
			t.Fatalf("queued leases=%d want 2", active)
		}
		time.Sleep(time.Millisecond)
	}
	closeErr := make(chan error, 1)
	go func() { closeErr <- a.Close(context.Background()) }()
	for {
		a.mu.Lock()
		state := a.state
		a.mu.Unlock()
		if state == appClosing {
			break
		}
		time.Sleep(time.Millisecond)
	}
	a.mutation.Unlock()
	if err := <-addErr; !errors.Is(err, ErrAppClosed) {
		t.Fatalf("queued AddProvider error=%v", err)
	}
	if err := <-invokeErr; !errors.Is(err, ErrAppClosed) {
		t.Fatalf("queued Invoke error=%v", err)
	}
	if err := <-closeErr; err != nil {
		t.Fatal(err)
	}
	ad.mu.Lock()
	discoverCalls, invokeCalls, closeCalls := ad.discoverCalls, ad.invokeCalls, ad.closeCalls
	ad.mu.Unlock()
	if discoverCalls != 1 || invokeCalls != 0 || closeCalls != 1 {
		t.Fatalf("calls discover=%d invoke=%d close=%d", discoverCalls, invokeCalls, closeCalls)
	}
	var endpoint string
	if err := a.db.QueryRowContext(ctx, `SELECT endpoint FROM providers WHERE id=?`, current.ID).Scan(&endpoint); err != nil {
		t.Fatal(err)
	}
	if endpoint != "current" {
		t.Fatalf("persisted endpoint=%q", endpoint)
	}
}

type lateStartAdapter struct {
	invokeEntered chan struct{}
	allowReturn   chan struct{}
	firstClose    chan struct{}
	mu            sync.Mutex
	closeCalls    int
	startedLate   bool
	reapedLate    bool
}

func (*lateStartAdapter) Discover(_ context.Context, p provider.Provider) ([]capability.Capability, error) {
	return []capability.Capability{lifecycleCapability(p.Protocol, p.Name)}, nil
}
func (a *lateStartAdapter) Invoke(_ context.Context, _ provider.Provider, _ capability.Capability, _ string, _ json.RawMessage, sink provider.Sink) error {
	close(a.invokeEntered)
	<-a.allowReturn
	a.mu.Lock()
	a.startedLate = true
	a.mu.Unlock()
	return sink.Event(provider.Event{Type: "result", Data: json.RawMessage(`{"ok":true}`)})
}
func (*lateStartAdapter) Cancel(context.Context, provider.Provider, string) error { return nil }
func (*lateStartAdapter) Health(context.Context, provider.Provider) provider.Health {
	return provider.Health{Healthy: true}
}
func (a *lateStartAdapter) Close(context.Context, provider.Provider) error {
	a.mu.Lock()
	a.closeCalls++
	calls := a.closeCalls
	if a.startedLate {
		a.reapedLate = true
	}
	a.mu.Unlock()
	if calls == 1 {
		close(a.firstClose)
	}
	return nil
}

func TestInvokeCleansSessionThatStartsAfterCloseSnapshot(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	ad := &lateStartAdapter{invokeEntered: make(chan struct{}), allowReturn: make(chan struct{}), firstClose: make(chan struct{})}
	a.mu.Lock()
	a.adapters["late"] = ad
	a.mu.Unlock()
	p := provider.Provider{ID: "late/shared", Protocol: "late", Name: "shared", Endpoint: "local"}
	if err := a.AddProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(ctx, "agent", "late/shared/route", []string{"invoke"}); err != nil {
		t.Fatal(err)
	}
	invokeErr := make(chan error, 1)
	go func() {
		_, err := a.Invoke(ctx, "agent", "late/shared/route", json.RawMessage(`{}`))
		invokeErr <- err
	}()
	<-ad.invokeEntered
	closeErr := make(chan error, 1)
	go func() { closeErr <- a.Close(ctx) }()
	<-ad.firstClose
	close(ad.allowReturn)
	if err := <-invokeErr; !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Invoke error=%v", err)
	}
	if err := <-closeErr; err != nil {
		t.Fatal(err)
	}
	ad.mu.Lock()
	closeCalls, reapedLate := ad.closeCalls, ad.reapedLate
	ad.mu.Unlock()
	if closeCalls != 2 || !reapedLate {
		t.Fatalf("closeCalls=%d reapedLate=%v", closeCalls, reapedLate)
	}
}
