package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/search"
	"github.com/gopact-ai/9a/internal/store"
)

const appAdapterHelperEnv = "NINEA_APP_ADAPTER_HELPER"

func TestAppAdapterHelperProcess(t *testing.T) {
	if os.Getenv(appAdapterHelperEnv) != "1" {
		return
	}
	if counter := os.Getenv("NINEA_APP_ADAPTER_COUNTER"); counter != "" {
		file, _ := os.OpenFile(counter, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if file != nil {
			_, _ = file.WriteString("start\n")
			_ = file.Close()
		}
	}
	if pids := os.Getenv("NINEA_APP_ADAPTER_PIDS"); pids != "" {
		file, _ := os.OpenFile(pids, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if file != nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			_ = file.Close()
		}
	}
	if pidPath := os.Getenv("NINEA_APP_ADAPTER_DESCENDANT_PID"); pidPath != "" {
		child := exec.Command("sh", "-c", "while :; do sleep 1; done")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(pidPath, []byte(fmt.Sprint(child.Process.Pid)), 0600)
	}
	type request struct {
		Version string          `json:"version"`
		ID      string          `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request request
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(4)
		}
		response := map[string]any{"version": "9a.adapter/v1", "id": request.ID}
		switch request.Method {
		case "discover":
			if os.Getenv("NINEA_APP_ADAPTER_MODE") == "discover-error" {
				response["error"] = map[string]any{"code": "discover_failed", "message": "unique discover failure"}
				break
			}
			response["result"] = map[string]any{"capabilities": []any{map[string]any{
				"upstream_name": "echo", "kind": "api.operation", "name": "Echo", "description": "Echo input",
				"input":  map[string]any{"mode": "json", "json_schema": map[string]any{"type": "object"}},
				"output": map[string]any{"mode": "json"}, "lifecycle": map[string]any{"sync": true},
				"security": map[string]any{"requires_approval": "never", "upstream_auth": "adapter-configured"},
			}}}
		case "invoke":
			if os.Getenv("NINEA_APP_ADAPTER_MODE") == "hang-invoke" {
				if ready := os.Getenv("NINEA_APP_ADAPTER_INVOKE_READY"); ready != "" {
					_ = os.WriteFile(ready, []byte("ready"), 0600)
				}
				continue
			}
			if os.Getenv("NINEA_APP_ADAPTER_MODE") == "same-name" {
				var params struct {
					Provider struct {
						Endpoint string `json:"endpoint"`
					} `json:"provider"`
				}
				_ = json.Unmarshal(request.Params, &params)
				response["result"] = map[string]any{"output": map[string]any{"endpoint": params.Provider.Endpoint}}
			} else {
				response["result"] = map[string]any{"output": map[string]any{"restored": true}}
			}
		case "health":
			response["result"] = map[string]any{"healthy": true, "message": fmt.Sprintf("pid=%d", os.Getpid())}
		default:
			response["error"] = map[string]any{"code": "unsupported_method", "message": "unsupported"}
		}
		_ = encoder.Encode(response)
	}
	os.Exit(0)
}

func appAdapterExecutable(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "app-adapter-helper")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestAppAdapterHelperProcess$\n", binary)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForPath(t *testing.T, path string) {
	t.Helper()
	for range 200 {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForPIDs(t *testing.T, path string, count int) []int {
	t.Helper()
	for range 200 {
		data, _ := os.ReadFile(path)
		lines := strings.Fields(string(data))
		if len(lines) == count {
			pids := make([]int, 0, count)
			for _, line := range lines {
				var pid int
				if _, err := fmt.Sscan(line, &pid); err != nil {
					t.Fatal(err)
				}
				pids = append(pids, pid)
			}
			return pids
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d helper pids", count)
	return nil
}

func assertProcessesReaped(t *testing.T, pids []int) {
	t.Helper()
	for _, pid := range pids {
		reaped := false
		for range 100 {
			if testProcessTerminated(pid) {
				reaped = true
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		if !reaped {
			t.Fatalf("helper process %d survived close", pid)
		}
	}
}

func testProcessTerminated(pid int) bool {
	if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
		return true
	}
	if runtime.GOOS != "linux" {
		return false
	}
	stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return os.IsNotExist(err)
	}
	marker := strings.LastIndex(string(stat), ") ")
	return marker >= 0 && len(stat) > marker+2 && stat[marker+2] == 'Z'
}

func TestExternalAdapterProviderRestoresSearchesAndInvokesAfterReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ninea.db")
	executable := appAdapterExecutable(t)
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(appAdapterHelperEnv, "1")
	t.Setenv("NINEA_APP_ADAPTER_COUNTER", counter)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	a := New(db)
	if err := a.AddAdapter(ctx, "echo", executable); err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "echo/demo", Protocol: "echo", Name: "demo", Endpoint: "local"}
	if err := a.AddProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	capabilityID := "echo/demo/echo"
	if err := a.Grant(ctx, "agent", capabilityID, []string{"read", "invoke"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a = New(db)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	results, err := a.Search(ctx, "agent", search.Query{Text: capabilityID})
	if err != nil || len(results) != 1 || results[0].Capability.ID != capabilityID {
		t.Fatalf("Search()=%#v, %v", results, err)
	}
	output, err := a.Invoke(ctx, "agent", capabilityID, json.RawMessage(`{"message":"hello"}`))
	if err != nil || string(output) != `{"restored":true}` {
		t.Fatalf("Invoke()=%s, %v", output, err)
	}
	starts, _ := os.ReadFile(counter)
	if string(starts) != "start\nstart\n" {
		t.Fatalf("helper starts=%q", starts)
	}
}

func TestSameNameProvidersRouteAndCloseByFullIDAcrossRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "ninea.db")
	executable := appAdapterExecutable(t)
	pidsPath := filepath.Join(t.TempDir(), "helper-pids")
	t.Setenv(appAdapterHelperEnv, "1")
	t.Setenv("NINEA_APP_ADAPTER_MODE", "same-name")
	t.Setenv("NINEA_APP_ADAPTER_PIDS", pidsPath)
	db, err := store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	a := New(db)
	if err := a.AddAdapter(ctx, "alpha", executable); err != nil {
		t.Fatal(err)
	}
	if err := a.AddAdapter(ctx, "beta", executable); err != nil {
		t.Fatal(err)
	}
	alpha := provider.Provider{ID: "alpha/shared", Protocol: "alpha", Name: "shared", Endpoint: "alpha-endpoint"}
	beta := provider.Provider{ID: "beta/shared", Protocol: "beta", Name: "shared", Endpoint: "beta-endpoint"}
	if err := a.AddProvider(ctx, alpha); err != nil {
		t.Fatal(err)
	}
	if err := a.AddProvider(ctx, beta); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	alphaAdapter := a.adapters["alpha"]
	betaAdapter := a.adapters["beta"]
	a.mu.RUnlock()
	defer alphaAdapter.Close(context.Background(), alpha)
	defer betaAdapter.Close(context.Background(), beta)
	for _, capabilityID := range []string{"alpha/shared/echo", "beta/shared/echo"} {
		if err := a.Grant(ctx, "agent", capabilityID, []string{"read", "invoke"}); err != nil {
			t.Fatal(err)
		}
	}
	assertEndpoint := func(app *App, capabilityID, endpoint string) {
		t.Helper()
		output, err := app.Invoke(ctx, "agent", capabilityID, json.RawMessage(`{"message":"hello"}`))
		if err != nil {
			t.Fatalf("Invoke(%s): %v", capabilityID, err)
		}
		var result struct {
			Endpoint string `json:"endpoint"`
		}
		if err := json.Unmarshal(output, &result); err != nil || result.Endpoint != endpoint {
			t.Fatalf("Invoke(%s)=%s, %v", capabilityID, output, err)
		}
	}
	assertEndpoint(a, "alpha/shared/echo", "alpha-endpoint")
	assertEndpoint(a, "beta/shared/echo", "beta-endpoint")
	firstPIDs := waitForPIDs(t, pidsPath, 2)
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertProcessesReaped(t, firstPIDs)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(pidsPath); err != nil {
		t.Fatal(err)
	}
	db, err = store.Open(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a = New(db)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	assertEndpoint(a, "alpha/shared/echo", "alpha-endpoint")
	assertEndpoint(a, "beta/shared/echo", "beta-endpoint")
	secondPIDs := waitForPIDs(t, pidsPath, 2)
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertProcessesReaped(t, secondPIDs)
}

type closeAdapter struct {
	mu          sync.Mutex
	closed      []string
	discoverErr error
	err         error
}

func (a *closeAdapter) Discover(context.Context, provider.Provider) ([]capability.Capability, error) {
	return nil, a.discoverErr
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
	if !errors.Is(err, firstErr) {
		t.Fatalf("Close() error=%v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Close() omitted context error: %v", err)
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

func TestAddProviderJoinsDiscoverAndCloseErrors(t *testing.T) {
	a, _ := testApp(t)
	discoverErr := errors.New("discover failed")
	closeErr := errors.New("close failed")
	ad := &closeAdapter{discoverErr: discoverErr, err: closeErr}
	a.mu.Lock()
	a.adapters["broken"] = ad
	a.mu.Unlock()
	p := provider.Provider{ID: "broken/demo", Protocol: "broken", Name: "demo"}
	err := a.AddProvider(context.Background(), p)
	if !errors.Is(err, discoverErr) || !errors.Is(err, closeErr) {
		t.Fatalf("AddProvider() error=%v", err)
	}
	ad.mu.Lock()
	closed := append([]string(nil), ad.closed...)
	ad.mu.Unlock()
	if len(closed) != 1 || closed[0] != "demo" {
		t.Fatalf("closed=%v", closed)
	}
	a.mu.RLock()
	_, installed := a.providers[p.ID]
	a.mu.RUnlock()
	if installed {
		t.Fatal("failed provider was installed")
	}
}

func TestAppCloseReapsExecutableAdapterDescendants(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	executable := appAdapterExecutable(t)
	descendantPath := filepath.Join(t.TempDir(), "descendant-pid")
	t.Setenv(appAdapterHelperEnv, "1")
	t.Setenv("NINEA_APP_ADAPTER_DESCENDANT_PID", descendantPath)
	if err := a.AddAdapter(ctx, "echo", executable); err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "echo/demo", Protocol: "echo", Name: "demo", Endpoint: "local"}
	if err := a.AddProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, descendantPath)
	data, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatal(err)
	}
	var descendantPID int
	if _, err := fmt.Sscan(string(data), &descendantPID); err != nil {
		t.Fatal(err)
	}
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := a.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	for range 100 {
		if testProcessTerminated(descendantPID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived App.Close", descendantPID)
}

func TestAddProviderPersistenceFailureReapsExecutableAdapterSession(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	executable := appAdapterExecutable(t)
	descendantPath := filepath.Join(t.TempDir(), "descendant-pid")
	t.Setenv(appAdapterHelperEnv, "1")
	t.Setenv("NINEA_APP_ADAPTER_DESCENDANT_PID", descendantPath)
	if err := a.AddAdapter(ctx, "echo", executable); err != nil {
		t.Fatal(err)
	}
	if _, err := a.db.ExecContext(ctx, `CREATE TRIGGER reject_provider BEFORE INSERT ON providers BEGIN SELECT RAISE(FAIL,'reject provider'); END`); err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "echo/demo", Protocol: "echo", Name: "demo", Endpoint: "local"}
	if err := a.AddProvider(ctx, p); err == nil {
		t.Fatal("AddProvider unexpectedly succeeded")
	}
	waitForPath(t, descendantPath)
	data, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatal(err)
	}
	var descendantPID int
	if _, err := fmt.Sscan(string(data), &descendantPID); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	ad := a.adapters["echo"]
	a.mu.RUnlock()
	defer ad.Close(context.Background(), p)
	for range 100 {
		if testProcessTerminated(descendantPID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived failed AddProvider", descendantPID)
}

func TestAddProviderDiscoverFailureReapsExecutableAdapterSession(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	executable := appAdapterExecutable(t)
	descendantPath := filepath.Join(t.TempDir(), "descendant-pid")
	t.Setenv(appAdapterHelperEnv, "1")
	t.Setenv("NINEA_APP_ADAPTER_MODE", "discover-error")
	t.Setenv("NINEA_APP_ADAPTER_DESCENDANT_PID", descendantPath)
	if err := a.AddAdapter(ctx, "echo", executable); err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "echo/demo", Protocol: "echo", Name: "demo", Endpoint: "local"}
	err := a.AddProvider(ctx, p)
	if err == nil || !strings.Contains(err.Error(), "unique discover failure") {
		t.Fatalf("AddProvider() error=%v", err)
	}
	waitForPath(t, descendantPath)
	data, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatal(err)
	}
	var descendantPID int
	if _, err := fmt.Sscan(string(data), &descendantPID); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	ad := a.adapters["echo"]
	_, byID := a.providers[p.ID]
	_, byName := a.providers[p.Name]
	a.mu.RUnlock()
	defer ad.Close(context.Background(), p)
	if byID || byName {
		t.Fatalf("failed provider installed: byID=%v byName=%v", byID, byName)
	}
	for range 100 {
		if testProcessTerminated(descendantPID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived Discover failure", descendantPID)
}

func TestRejectedRestoreLeavesActiveExternalSessionOwnedByAppClose(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	executable := appAdapterExecutable(t)
	descendantPath := filepath.Join(t.TempDir(), "descendant-pid")
	t.Setenv(appAdapterHelperEnv, "1")
	t.Setenv("NINEA_APP_ADAPTER_DESCENDANT_PID", descendantPath)
	if err := a.AddAdapter(ctx, "echo", executable); err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "echo/demo", Protocol: "echo", Name: "demo", Endpoint: "local"}
	if err := a.AddProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, descendantPath)
	data, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatal(err)
	}
	var descendantPID int
	if _, err := fmt.Sscan(string(data), &descendantPID); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	beforeAdapter := a.adapters["echo"]
	beforeProvider := a.providers[p.ID]
	a.mu.RUnlock()
	defer beforeAdapter.Close(context.Background(), p)
	if err := a.Restore(ctx); !errors.Is(err, ErrRestoreRequiresFreshApp) {
		t.Fatalf("Restore() error=%v", err)
	}
	a.mu.RLock()
	afterAdapter := a.adapters["echo"]
	afterProvider := a.providers[p.ID]
	a.mu.RUnlock()
	if afterAdapter != beforeAdapter || afterProvider.ID != beforeProvider.ID || afterProvider.Endpoint != beforeProvider.Endpoint {
		t.Fatal("rejected Restore mutated active adapter/provider maps")
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	assertProcessesReaped(t, []int{descendantPID})
}

func TestCloseCancelsHungInvokeAndReapsProcessWithoutWaitingForLifecycleGate(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	executable := appAdapterExecutable(t)
	descendantPath := filepath.Join(t.TempDir(), "descendant-pid")
	invokeReady := filepath.Join(t.TempDir(), "invoke-ready")
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(appAdapterHelperEnv, "1")
	t.Setenv("NINEA_APP_ADAPTER_MODE", "hang-invoke")
	t.Setenv("NINEA_APP_ADAPTER_DESCENDANT_PID", descendantPath)
	t.Setenv("NINEA_APP_ADAPTER_INVOKE_READY", invokeReady)
	t.Setenv("NINEA_APP_ADAPTER_COUNTER", counter)
	if err := a.AddAdapter(ctx, "echo", executable); err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "echo/demo", Protocol: "echo", Name: "demo", Endpoint: "local"}
	if err := a.AddProvider(ctx, p); err != nil {
		t.Fatal(err)
	}
	if err := a.Grant(ctx, "agent", "echo/demo/echo", []string{"invoke"}); err != nil {
		t.Fatal(err)
	}
	waitForPath(t, descendantPath)
	data, err := os.ReadFile(descendantPath)
	if err != nil {
		t.Fatal(err)
	}
	var descendantPID int
	if _, err := fmt.Sscan(string(data), &descendantPID); err != nil {
		t.Fatal(err)
	}
	a.mu.RLock()
	ad := a.adapters["echo"]
	a.mu.RUnlock()
	defer ad.Close(context.Background(), p)
	invokeErr := make(chan error, 1)
	go func() {
		_, err := a.Invoke(context.Background(), "agent", "echo/demo/echo", json.RawMessage(`{}`))
		invokeErr <- err
	}()
	waitForPath(t, invokeReady)
	closeCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	closeErr := make(chan error, 1)
	go func() { closeErr <- a.Close(closeCtx) }()
	select {
	case <-closeErr:
	case <-time.After(time.Second):
		t.Fatal("App.Close blocked behind hung Invoke before initiating adapter termination")
	}
	select {
	case err := <-invokeErr:
		if err == nil {
			t.Fatal("hung Invoke succeeded during Close")
		}
	case <-time.After(time.Second):
		t.Fatal("hung Invoke did not unwind after Close")
	}
	assertProcessesReaped(t, []int{descendantPID})
	starts, _ := os.ReadFile(counter)
	if string(starts) != "start\n" {
		t.Fatalf("helper starts=%q", starts)
	}
	if _, err := a.Invoke(context.Background(), "agent", "echo/demo/echo", json.RawMessage(`{}`)); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Invoke after Close error=%v", err)
	}
	if err := a.AddProvider(context.Background(), p); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("AddProvider after Close error=%v", err)
	}
	if err := a.AddAdapter(context.Background(), "other", testExecutable(t, "other-adapter")); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("AddAdapter after Close error=%v", err)
	}
	if err := a.Restore(context.Background()); !errors.Is(err, ErrAppClosed) {
		t.Fatalf("Restore after Close error=%v", err)
	}
	starts, _ = os.ReadFile(counter)
	if string(starts) != "start\n" {
		t.Fatalf("post-close operation restarted helper: %q", starts)
	}
	second := make(chan error, 1)
	go func() { second <- a.Close(context.Background()) }()
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("repeated Close was not bounded")
	}
}
