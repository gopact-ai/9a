package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
	executableprovider "github.com/gopact-ai/9a/internal/provider/executable"
)

type asyncTestAdapter struct {
	cancelable bool
	blocking   bool
	started    chan struct{}
	allowStart chan struct{}
	canceled   chan struct{}
	cancelSeen chan struct{}
	invokeErr  error
	cancelErr  error
	once       sync.Once
}

type gatedExecutableAdapter struct {
	provider.Adapter
	entered    chan struct{}
	release    chan struct{}
	cancelSeen chan struct{}
}

func (a *gatedExecutableAdapter) Invoke(ctx context.Context, p provider.Provider, c capability.Capability, id string, input json.RawMessage, sink provider.Sink) error {
	close(a.entered)
	select {
	case <-a.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return a.Adapter.Invoke(ctx, p, c, id, input, sink)
}

func (a *gatedExecutableAdapter) Cancel(ctx context.Context, p provider.Provider, id string) error {
	select {
	case <-a.cancelSeen:
	default:
		close(a.cancelSeen)
	}
	return a.Adapter.Cancel(ctx, p, id)
}

func (a *asyncTestAdapter) Discover(_ context.Context, p provider.Provider) ([]capability.Capability, error) {
	return []capability.Capability{{
		ID: p.Protocol + "/" + p.Name + "/async", Kind: "api.operation", Name: "Async", Description: "Async operation",
		Source: capability.Source{Protocol: p.Protocol, Provider: p.Name, UpstreamName: "async"},
		Input:  capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "json"},
		Lifecycle: capability.Lifecycle{Sync: true, Streaming: true, Cancelable: a.cancelable},
	}}, nil
}

func (a *asyncTestAdapter) Invoke(ctx context.Context, _ provider.Provider, _ capability.Capability, _ string, _ json.RawMessage, sink provider.Sink) error {
	if a.started != nil {
		a.once.Do(func() { close(a.started) })
	}
	if a.allowStart != nil {
		select {
		case <-a.allowStart:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := sink.Started(); err != nil {
		return err
	}
	if a.invokeErr != nil {
		return a.invokeErr
	}
	if a.blocking {
		select {
		case <-a.canceled:
			err, _ := provider.NewAdapterError("canceled", "invocation canceled")
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err := sink.Event(provider.Event{Type: "progress", Data: json.RawMessage(`{"step":1}`)}); err != nil {
		return err
	}
	if err := sink.Artifact("report.txt", "text/plain", []byte("artifact")); err != nil {
		return err
	}
	return sink.Event(provider.Event{Type: "result", Data: json.RawMessage(`{"ok":true}`)})
}

func TestZeroValueAdapterErrorFromInvokeBecomesInternalError(t *testing.T) {
	adapter := &asyncTestAdapter{cancelable: true, allowStart: make(chan struct{}), canceled: make(chan struct{}), invokeErr: &provider.AdapterError{}}
	a, _, capabilityID := setupAsyncApp(t, adapter)
	id, err := a.StartCall(context.Background(), "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	runtime := a.activeCalls[id]
	a.mu.Unlock()
	close(adapter.allowStart)
	record := waitCallState(t, a, "owner", id, call.Failed)
	if record.Call.Code != "internal_error" || record.Call.Message == "" {
		t.Fatalf("record=%#v", record)
	}
	<-runtime.done
	adapter.invokeErr = nil
	nextID, err := a.StartCall(context.Background(), "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = waitCallState(t, a, "owner", nextID, call.Completed)
}

func TestAggregateEventBudgetFailureUsesEventLimitTerminalCode(t *testing.T) {
	adapter := &asyncTestAdapter{cancelable: true, canceled: make(chan struct{}), invokeErr: call.ErrEventBudgetExceeded}
	a, _, capabilityID := setupAsyncApp(t, adapter)
	id, err := a.StartCall(context.Background(), "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	record := waitCallState(t, a, "owner", id, call.Failed)
	if record.Call.Code != "event_limit" {
		t.Fatalf("record=%#v", record)
	}
}

func TestTypedNilAdapterErrorFromInvokeHelper(t *testing.T) {
	if os.Getenv("NINEA_TYPED_NIL_INVOKE_HELPER") != "1" {
		return
	}
	var typedNil *provider.AdapterError
	var invokeErr error = typedNil
	adapter := &asyncTestAdapter{cancelable: true, allowStart: make(chan struct{}), canceled: make(chan struct{}), invokeErr: invokeErr}
	a, _, capabilityID := setupAsyncApp(t, adapter)
	id, err := a.StartCall(context.Background(), "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	runtime := a.activeCalls[id]
	a.mu.Unlock()
	close(adapter.allowStart)
	record := waitCallState(t, a, "owner", id, call.Failed)
	if record.Call.Code != "internal_error" || record.Call.Message == "" {
		t.Fatalf("record=%#v", record)
	}
	<-runtime.done
	adapter.invokeErr = nil
	nextID, err := a.StartCall(context.Background(), "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = waitCallState(t, a, "owner", nextID, call.Completed)
}

func TestTypedNilAdapterErrorFromInvokeDoesNotCrash(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestTypedNilAdapterErrorFromInvokeHelper$")
	cmd.Env = append(os.Environ(), "NINEA_TYPED_NIL_INVOKE_HELPER=1")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("typed-nil Invoke crashed helper: %v\n%s", err, output)
	}
}

func (a *asyncTestAdapter) Cancel(context.Context, provider.Provider, string) error {
	if a.cancelSeen != nil {
		select {
		case <-a.cancelSeen:
		default:
			close(a.cancelSeen)
		}
	}
	if a.cancelErr != nil {
		return a.cancelErr
	}
	if !a.cancelable {
		err, _ := provider.NewAdapterError("not_cancelable", "invocation is not cancelable")
		return err
	}
	select {
	case <-a.canceled:
	default:
		close(a.canceled)
	}
	return nil
}

func TestInvalidAdapterErrorsFromCancelReturnStableFailure(t *testing.T) {
	var typedNil *provider.AdapterError
	for name, cancelErr := range map[string]error{
		"zero value": &provider.AdapterError{},
		"typed nil":  typedNil,
	} {
		t.Run(name, func(t *testing.T) {
			adapter := &asyncTestAdapter{cancelable: true, blocking: true, started: make(chan struct{}), canceled: make(chan struct{}), cancelErr: cancelErr}
			a, _, capabilityID := setupAsyncApp(t, adapter)
			id, err := a.StartCall(context.Background(), "owner", capabilityID, json.RawMessage(`{}`))
			if err != nil {
				t.Fatal(err)
			}
			<-adapter.started
			if err := a.CancelCall(context.Background(), "owner", id); err == nil || err.Error() != "adapter cancellation failed" {
				t.Fatalf("CancelCall error=%T %v", err, err)
			}
			adapter.cancelErr = nil
			if err := a.CancelCall(context.Background(), "owner", id); err != nil {
				t.Fatalf("subsequent CancelCall error=%v", err)
			}
			_ = waitCallState(t, a, "owner", id, call.Canceled)
		})
	}
}

func TestCancelWaitsForInvocationReadiness(t *testing.T) {
	ctx := context.Background()
	adapter := &asyncTestAdapter{
		cancelable: true,
		blocking:   true,
		started:    make(chan struct{}),
		allowStart: make(chan struct{}),
		canceled:   make(chan struct{}),
		cancelSeen: make(chan struct{}),
	}
	a, _, capabilityID := setupAsyncApp(t, adapter)
	id, err := a.StartCall(ctx, "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-adapter.started
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- a.CancelCall(ctx, "owner", id) }()
	waitAppLeaseCount(t, a, 2)
	select {
	case err := <-cancelDone:
		t.Fatalf("CancelCall returned before readiness: %v", err)
	case <-adapter.cancelSeen:
		t.Fatal("adapter Cancel called before readiness")
	default:
	}
	close(adapter.allowStart)
	if err := <-cancelDone; err != nil {
		t.Fatalf("CancelCall after readiness: %v", err)
	}
	select {
	case <-adapter.cancelSeen:
	default:
		t.Fatal("adapter Cancel was not called after readiness")
	}
	record := waitCallState(t, a, "owner", id, call.Canceled)
	if record.Call.Code != "canceled" {
		t.Fatalf("record=%#v", record)
	}
}

func TestImmediateCancelWaitsForRealExecutablePendingRegistration(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "execfixture")
	build := exec.Command("go", "build", "-o", fixture, "../../testdata/executableadapter")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build executable fixture: %v\n%s", err, output)
	}
	external, err := executableprovider.New("exec", fixture)
	if err != nil {
		t.Fatal(err)
	}
	gated := &gatedExecutableAdapter{Adapter: external, entered: make(chan struct{}), release: make(chan struct{}), cancelSeen: make(chan struct{})}
	a, _ := testApp(t)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	a.mu.Lock()
	a.adapters["exec"] = gated
	a.mu.Unlock()
	p := provider.Provider{ID: "exec/demo", Protocol: "exec", Name: "demo", Endpoint: "local"}
	if err := a.AddProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	capabilityID := "exec/demo/async"
	if err := a.Grant(context.Background(), "owner", capabilityID, []string{"invoke"}); err != nil {
		t.Fatal(err)
	}
	id, err := a.StartCall(context.Background(), "owner", capabilityID, json.RawMessage(`{"block":true}`))
	if err != nil {
		t.Fatal(err)
	}
	<-gated.entered
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- a.CancelCall(context.Background(), "owner", id) }()
	waitAppLeaseCount(t, a, 2)
	select {
	case err := <-cancelDone:
		t.Fatalf("CancelCall returned before external pending registration: %v", err)
	case <-gated.cancelSeen:
		t.Fatal("external Cancel called before pending registration")
	default:
	}
	close(gated.release)
	if err := <-cancelDone; err != nil {
		t.Fatalf("CancelCall after external pending registration: %v", err)
	}
	record := waitCallState(t, a, "owner", id, call.Canceled)
	if record.Call.Code != "canceled" {
		t.Fatalf("record=%#v", record)
	}
}
func (*asyncTestAdapter) Health(context.Context, provider.Provider) provider.Health {
	return provider.Health{Healthy: true}
}
func (*asyncTestAdapter) Close(context.Context, provider.Provider) error { return nil }

func setupAsyncApp(t *testing.T, adapter *asyncTestAdapter) (*App, provider.Provider, string) {
	t.Helper()
	a, _ := testApp(t)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	a.mu.Lock()
	a.adapters["async"] = adapter
	a.mu.Unlock()
	p := provider.Provider{ID: "async/demo", Protocol: "async", Name: "demo", Endpoint: "local"}
	if err := a.AddProvider(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	capabilityID := "async/demo/async"
	if err := a.Grant(context.Background(), "owner", capabilityID, []string{"invoke"}); err != nil {
		t.Fatal(err)
	}
	return a, p, capabilityID
}

func appSubmittedCall(id string) call.Call {
	return call.Call{ID: id, CapabilityID: "echo/demo/echo", IdentityID: "owner", State: call.Submitted}
}

func waitCallState(t *testing.T, a *App, identity, id string, terminal ...call.State) call.Record {
	t.Helper()
	wanted := map[call.State]bool{}
	for _, state := range terminal {
		wanted[state] = true
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		record, err := a.GetCall(context.Background(), identity, id)
		if err == nil && wanted[record.Call.State] {
			return record
		}
		if time.Now().After(deadline) {
			t.Fatalf("call %s did not reach %v; record=%#v err=%v", id, terminal, record, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitAppLeaseCount(t *testing.T, a *App, want int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); ; {
		a.mu.Lock()
		count := len(a.leases)
		a.mu.Unlock()
		if count >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("operation lease count=%d, want at least %d", count, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestStartCallPersistsInputEventsArtifactsAndResult(t *testing.T) {
	ctx := context.Background()
	a, _, capabilityID := setupAsyncApp(t, &asyncTestAdapter{cancelable: true, canceled: make(chan struct{})})
	input := json.RawMessage(`{"provider_sensitive":"value"}`)
	id, err := a.StartCall(ctx, "owner", capabilityID, input)
	if err != nil || id == "" {
		t.Fatalf("StartCall() id=%q err=%v", id, err)
	}
	var storedInput []byte
	if err := a.db.QueryRowContext(ctx, `SELECT data_json FROM call_inputs WHERE call_id=?`, id).Scan(&storedInput); err != nil || string(storedInput) != string(input) {
		t.Fatalf("stored input=%s err=%v", storedInput, err)
	}
	record := waitCallState(t, a, "owner", id, call.Completed)
	if string(record.Result) != `{"ok":true}` || record.Call.IdentityID != "owner" {
		t.Fatalf("record=%#v", record)
	}
	events, err := a.ListCallEvents(ctx, "owner", id)
	if err != nil || len(events) != 3 {
		t.Fatalf("events=%#v err=%v", events, err)
	}
	var kinds []string
	for i, event := range events {
		var envelope struct {
			Kind      string          `json:"kind"`
			Encoding  string          `json:"encoding"`
			Data      json.RawMessage `json:"data"`
			MediaType string          `json:"media_type"`
		}
		if err := json.Unmarshal(event.Envelope, &envelope); err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, envelope.Kind)
		if event.Sequence != i+1 {
			t.Fatalf("event sequence=%d", event.Sequence)
		}
		if envelope.Kind == "artifact" {
			var encoded string
			_ = json.Unmarshal(envelope.Data, &encoded)
			if envelope.Encoding != "base64" || encoded != "YXJ0aWZhY3Q=" || envelope.MediaType != "text/plain" {
				t.Fatalf("artifact envelope=%s", event.Envelope)
			}
		}
	}
	if got := stringsJoin(kinds); got != "event,artifact,event" {
		t.Fatalf("event kinds=%s", got)
	}
}

func stringsJoin(values []string) string {
	var out string
	for i, value := range values {
		if i != 0 {
			out += ","
		}
		out += value
	}
	return out
}

func TestCancelCallAndOwnershipAreNonDisclosing(t *testing.T) {
	ctx := context.Background()
	adapter := &asyncTestAdapter{cancelable: true, blocking: true, started: make(chan struct{}), canceled: make(chan struct{})}
	a, _, capabilityID := setupAsyncApp(t, adapter)
	if err := a.Bootstrap(ctx, "root-token"); err != nil {
		t.Fatal(err)
	}
	id, err := a.StartCall(ctx, "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-adapter.started
	if _, err := a.GetCall(ctx, "other", id); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("other GetCall error=%v", err)
	}
	if _, err := a.ListCallEvents(ctx, "other", id); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("other ListCallEvents error=%v", err)
	}
	if err := a.CancelCall(ctx, "other", id); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("other CancelCall error=%v", err)
	}
	if _, err := a.GetCall(ctx, "admin", id); err != nil {
		t.Fatalf("admin GetCall error=%v", err)
	}
	if _, err := a.ListCallEventPage(ctx, "admin", id, 0, 1); err != nil {
		t.Fatalf("admin ListCallEventPage error=%v", err)
	}
	if err := a.CancelCall(ctx, "owner", id); err != nil {
		t.Fatalf("CancelCall: %v", err)
	}
	record := waitCallState(t, a, "owner", id, call.Canceled)
	if record.Call.Code != "canceled" {
		t.Fatalf("canceled record=%#v", record)
	}
	if err := a.CancelCall(ctx, "owner", id); !errors.Is(err, ErrCallNotActive) {
		t.Fatalf("second CancelCall error=%v", err)
	}
}

func TestStartCallValidationAndNotCancelable(t *testing.T) {
	ctx := context.Background()
	adapter := &asyncTestAdapter{blocking: true, started: make(chan struct{}), canceled: make(chan struct{})}
	a, _, capabilityID := setupAsyncApp(t, adapter)
	if _, err := a.StartCall(ctx, "owner", capabilityID, json.RawMessage(`not-json`)); err == nil {
		t.Fatal("StartCall accepted invalid JSON")
	}
	oversized := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, call.MaxPayloadBytes)...)
	oversized = append(oversized, '"')
	if _, err := a.StartCall(ctx, "owner", capabilityID, oversized); !errors.Is(err, call.ErrPayloadTooLarge) {
		t.Fatalf("oversized StartCall error=%v", err)
	}
	id, err := a.StartCall(ctx, "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-adapter.started
	if err := a.CancelCall(ctx, "owner", id); !errors.Is(err, ErrCallNotCancelable) {
		t.Fatalf("CancelCall error=%v", err)
	}
}

func TestRestoreMarksInterruptedCallsFailedWithoutChangingTerminal(t *testing.T) {
	ctx := context.Background()
	a, _ := testApp(t)
	repo := call.NewRepository(a.db)
	working := appSubmittedCall("call-working-restore")
	working.State = call.Working
	completed := appSubmittedCall("call-completed-restore")
	completed.State = call.Completed
	if err := repo.Create(ctx, working, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := repo.Create(ctx, completed, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	if err := a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	gotWorking, _ := repo.Get(ctx, working.ID)
	gotCompleted, _ := repo.Get(ctx, completed.ID)
	if gotWorking.Call.State != call.Failed || gotWorking.Call.Code != "daemon_restarted" || gotCompleted.Call.State != call.Completed {
		t.Fatalf("working=%#v completed=%#v", gotWorking.Call, gotCompleted.Call)
	}
}

func TestPersistentCallSinkAcceptsEightMiBArtifactButRejectsLarger(t *testing.T) {
	a, _ := testApp(t)
	repo := call.NewRepository(a.db)
	if err := repo.Create(context.Background(), appSubmittedCall("call-artifact-bound"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	sink := &persistentCallSink{repo: repo, callID: "call-artifact-bound"}
	if err := sink.Artifact("max.bin", "application/octet-stream", make([]byte, call.MaxPayloadBytes)); err != nil {
		t.Fatalf("8 MiB artifact error=%v", err)
	}
	if err := sink.Artifact("too-large.bin", "application/octet-stream", make([]byte, call.MaxPayloadBytes+1)); !errors.Is(err, call.ErrPayloadTooLarge) {
		t.Fatalf("oversized artifact error=%v", err)
	}
}

func TestEightMiBArtifactsExhaustPerCallAggregateBudget(t *testing.T) {
	a, _ := testApp(t)
	repo := call.NewRepository(a.db)
	if err := repo.Create(context.Background(), appSubmittedCall("call-artifact-aggregate"), json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	sink := &persistentCallSink{repo: repo, callID: "call-artifact-aggregate"}
	artifact := make([]byte, call.MaxPayloadBytes)
	successes := 0
	var terminalErr error
	for range 10 {
		terminalErr = sink.Artifact("max.bin", "application/octet-stream", artifact)
		if terminalErr != nil {
			break
		}
		successes++
	}
	if successes != 5 || !errors.Is(terminalErr, call.ErrEventBudgetExceeded) {
		t.Fatalf("successful artifacts=%d terminal error=%v", successes, terminalErr)
	}
	var count, bytes int64
	if err := a.db.QueryRow(`SELECT event_count,byte_count FROM call_event_usage WHERE call_id=?`, "call-artifact-aggregate").Scan(&count, &bytes); err != nil {
		t.Fatal(err)
	}
	if count != int64(successes) || bytes > call.MaxEventAggregateBytes {
		t.Fatalf("usage count=%d bytes=%d", count, bytes)
	}
}

func TestCompletePersistenceFailureFallsBackToInternalError(t *testing.T) {
	ctx := context.Background()
	a, _, capabilityID := setupAsyncApp(t, &asyncTestAdapter{cancelable: true, canceled: make(chan struct{})})
	if _, err := a.db.ExecContext(ctx, `CREATE TRIGGER reject_completed_call BEFORE UPDATE OF state ON calls WHEN NEW.state='completed' BEGIN SELECT RAISE(FAIL,'reject completed state'); END`); err != nil {
		t.Fatal(err)
	}
	id, err := a.StartCall(ctx, "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	record := waitCallState(t, a, "owner", id, call.Failed)
	if record.Call.Code != "internal_error" || record.Call.Message != "call terminal state persistence failed" || record.Result != nil {
		t.Fatalf("record=%#v", record)
	}
}

func TestUnrecoverableTerminalPersistenceErrorIsRetainedWithoutBlockingShutdown(t *testing.T) {
	ctx := context.Background()
	a, _, capabilityID := setupAsyncApp(t, &asyncTestAdapter{cancelable: true, canceled: make(chan struct{})})
	if _, err := a.db.ExecContext(ctx, `CREATE TRIGGER reject_all_terminal_calls BEFORE UPDATE OF state ON calls WHEN NEW.state IN ('completed','failed','canceled','rejected') BEGIN SELECT RAISE(FAIL,'reject every terminal state'); END`); err != nil {
		t.Fatal(err)
	}
	id, err := a.StartCall(ctx, "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	runtime := a.activeCalls[id]
	a.mu.Unlock()
	if runtime == nil {
		t.Fatal("runtime missing immediately after StartCall")
	}
	<-runtime.done
	if _, err := a.GetCall(ctx, "other", id); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("unauthorized GetCall error=%v", err)
	}
	if _, err := a.GetCall(ctx, "owner", id); err == nil || err.Error() != "call terminal state persistence failed" {
		t.Fatalf("GetCall terminal persistence error=%v", err)
	}
	if err := a.CancelCall(ctx, "owner", id); err == nil || err.Error() != "call terminal state persistence failed" {
		t.Fatalf("CancelCall terminal persistence error=%v", err)
	}
	record, err := a.callDB.Get(ctx, id)
	if err != nil || record.Call.State != call.Working {
		t.Fatalf("unpersisted terminal record=%#v err=%v", record, err)
	}
	if _, err := a.db.ExecContext(ctx, `DROP TRIGGER reject_all_terminal_calls`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.callDB.RecoverInterrupted(ctx, "daemon_restarted", "daemon restarted before call completed"); err != nil {
		t.Fatal(err)
	}
	recovered, err := a.callDB.Get(ctx, id)
	if err != nil || recovered.Call.State != call.Failed || recovered.Call.Code != "daemon_restarted" {
		t.Fatalf("recovered record=%#v err=%v", recovered, err)
	}
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := a.Close(closeCtx); err != nil {
		t.Fatalf("Close after persistence failure: %v", err)
	}
}

func TestRuntimePersistenceErrorRetentionIsBounded(t *testing.T) {
	a, _ := testApp(t)
	for i := range maxRuntimeCallErrors + 10 {
		a.recordCallError(fmt.Sprintf("call-error-%d", i), errors.New("database unavailable"))
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.callErrors) != maxRuntimeCallErrors || len(a.callErrorOrder) != maxRuntimeCallErrors {
		t.Fatalf("callErrors=%d order=%d", len(a.callErrors), len(a.callErrorOrder))
	}
	if _, exists := a.callErrors["call-error-0"]; exists {
		t.Fatal("oldest runtime error was not evicted")
	}
	if _, exists := a.callErrors[fmt.Sprintf("call-error-%d", maxRuntimeCallErrors+9)]; !exists {
		t.Fatal("newest runtime error was not retained")
	}
}

func TestCancelReportsTerminalPersistenceFailureAfterWorkerDone(t *testing.T) {
	ctx := context.Background()
	adapter := &asyncTestAdapter{cancelable: true, blocking: true, started: make(chan struct{}), canceled: make(chan struct{})}
	a, _, capabilityID := setupAsyncApp(t, adapter)
	if err := a.Bootstrap(ctx, "root-token"); err != nil {
		t.Fatal(err)
	}
	id, err := a.StartCall(ctx, "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-adapter.started
	if _, err := a.db.ExecContext(ctx, `CREATE TRIGGER reject_cancel_terminal BEFORE UPDATE OF state ON calls WHEN NEW.state IN ('canceled','failed') BEGIN SELECT RAISE(FAIL,'reject cancel terminal state'); END`); err != nil {
		t.Fatal(err)
	}
	if err := a.CancelCall(ctx, "owner", id); !errors.Is(err, ErrCallPersistence) {
		t.Fatalf("CancelCall error=%v", err)
	}
	if _, err := a.GetCall(ctx, "other", id); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("other GetCall error=%v", err)
	}
	for _, identity := range []string{"owner", "admin"} {
		if _, err := a.GetCall(ctx, identity, id); !errors.Is(err, ErrCallPersistence) {
			t.Fatalf("%s GetCall error=%v", identity, err)
		}
		if err := a.CancelCall(ctx, identity, id); !errors.Is(err, ErrCallPersistence) {
			t.Fatalf("%s subsequent CancelCall error=%v", identity, err)
		}
	}
	closeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := a.Close(closeCtx); err != nil {
		t.Fatalf("Close after cancel persistence failure: %v", err)
	}
}

func TestCancelPersistencePublicationWinsBeforeRuntimeSnapshot(t *testing.T) {
	ctx := context.Background()
	adapter := &asyncTestAdapter{cancelable: true, blocking: true, started: make(chan struct{}), canceled: make(chan struct{})}
	a, _, capabilityID := setupAsyncApp(t, adapter)
	id, err := a.StartCall(ctx, "owner", capabilityID, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	<-adapter.started
	a.mu.Lock()
	runtime := a.activeCalls[id]
	a.mu.Unlock()
	if runtime == nil {
		t.Fatal("active runtime missing")
	}
	if _, err := a.db.ExecContext(ctx, `CREATE TRIGGER reject_interleaved_cancel_terminal BEFORE UPDATE OF state ON calls WHEN NEW.state IN ('canceled','failed') BEGIN SELECT RAISE(FAIL,'reject interleaved cancel terminal state'); END`); err != nil {
		t.Fatal(err)
	}
	authorized := make(chan struct{})
	release := make(chan struct{})
	a.cancelBeforeRuntimeSnapshot = func() {
		close(authorized)
		<-release
	}
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- a.CancelCall(ctx, "owner", id) }()
	<-authorized
	close(adapter.canceled)
	<-runtime.done
	close(release)
	if err := <-cancelDone; !errors.Is(err, ErrCallPersistence) {
		t.Fatalf("CancelCall error=%v", err)
	}
	if _, err := a.GetCall(ctx, "other", id); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("unauthorized GetCall error=%v", err)
	}
}
