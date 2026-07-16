package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/jsoncontract"
	"github.com/gopact-ai/9a/internal/provider"
)

func TestRunResolvesShortReferenceAndPersistsCall(t *testing.T) {
	a, p, capabilityID := setupAsyncApp(t, &asyncTestAdapter{cancelable: true, canceled: make(chan struct{})})

	result, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", json.RawMessage(`{"city":"Shanghai"}`), "")
	if err != nil || string(result) != `{"ok":true}` {
		t.Fatalf("Run()=%s, %v", result, err)
	}

	var id, persistedCapability, state string
	if err := a.db.QueryRow(`SELECT id,capability_id,state FROM calls`).Scan(&id, &persistedCapability, &state); err != nil {
		t.Fatal(err)
	}
	if id == "" || persistedCapability != capabilityID || state != string(call.Completed) {
		t.Fatalf("persisted call id=%q capability=%q state=%q", id, persistedCapability, state)
	}
}

func TestRunRejectsInvalidInputBeforeUpstreamCall(t *testing.T) {
	started := make(chan struct{})
	adapter := &asyncTestAdapter{
		cancelable:       true,
		canceled:         make(chan struct{}),
		started:          started,
		requiresApproval: true,
		inputSchema: map[string]any{
			"type":     "object",
			"required": []any{"city"},
			"properties": map[string]any{
				"city": map[string]any{"type": "string"},
			},
		},
	}
	a, p, _ := setupAsyncApp(t, adapter)

	_, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", json.RawMessage(`{"city":1}`), "")
	if !errors.Is(err, jsoncontract.ErrInvalidValue) {
		t.Fatalf("Run() error=%v", err)
	}
	select {
	case <-started:
		t.Fatal("invalid input reached the upstream adapter")
	default:
	}
	var calls int
	if err := a.db.QueryRow(`SELECT count(*) FROM calls`).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("persisted invalid call count=%d err=%v", calls, err)
	}
}

func TestRunPersistsInvalidOutputAsFailure(t *testing.T) {
	adapter := &asyncTestAdapter{
		cancelable: true,
		canceled:   make(chan struct{}),
		outputSchema: map[string]any{
			"type":     "object",
			"required": []any{"ok"},
			"properties": map[string]any{
				"ok": map[string]any{"type": "string"},
			},
		},
	}
	a, p, _ := setupAsyncApp(t, adapter)

	_, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", json.RawMessage(`{}`), "")
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Code != "invalid_output" || runErr.CallID == "" {
		t.Fatalf("Run() error=%#v", err)
	}
	record, getErr := a.getCall(context.Background(), "owner", runErr.CallID)
	if getErr != nil || record.Call.State != call.Failed {
		t.Fatalf("record=%#v err=%v", record, getErr)
	}
}

func TestRunFailureReturnsPersistentCallIDAndCode(t *testing.T) {
	invokeErr, err := provider.NewAdapterError("upstream_failed", "upstream unavailable")
	if err != nil {
		t.Fatal(err)
	}
	a, p, _ := setupAsyncApp(t, &asyncTestAdapter{cancelable: true, canceled: make(chan struct{}), invokeErr: invokeErr})

	_, err = a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", json.RawMessage(`{}`), "")
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.CallID == "" || runErr.Code != "upstream_failed" {
		t.Fatalf("Run() error=%#v", err)
	}
	record, getErr := a.getCall(context.Background(), "owner", runErr.CallID)
	if getErr != nil || record.Call.State != call.Failed || record.Call.Code != runErr.Code {
		t.Fatalf("persisted failure=%#v, %v", record, getErr)
	}
}

func TestRunRequiresExplicitApprovalBeforeCreatingCall(t *testing.T) {
	started := make(chan struct{})
	adapter := &asyncTestAdapter{
		cancelable:       true,
		canceled:         make(chan struct{}),
		started:          started,
		requiresApproval: true,
	}
	a, p, _ := setupAsyncApp(t, adapter)

	input := json.RawMessage(`{}`)
	_, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", input, "")
	var approval *ApprovalRequiredError
	if !errors.As(err, &approval) || approval.Capability != "demo/async" || approval.Token == "" {
		t.Fatalf("Run() error=%#v", err)
	}
	select {
	case <-started:
		t.Fatal("unapproved run reached the upstream adapter")
	default:
	}
	var calls int
	if err := a.db.QueryRow(`SELECT count(*) FROM calls`).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("unapproved calls=%d err=%v", calls, err)
	}

	result, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", input, approval.Token)
	if err != nil || string(result) != `{"ok":true}` {
		t.Fatalf("approved Run()=%s, %v", result, err)
	}
}

func TestRunRejectsReplayedApproval(t *testing.T) {
	adapter := &asyncTestAdapter{cancelable: true, canceled: make(chan struct{}), requiresApproval: true}
	a, p, _ := setupAsyncApp(t, adapter)
	input := json.RawMessage(`{}`)
	token := approvalForRun(t, a, "owner", p.Config["workspace_root"], "demo/async", input)

	if _, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", input, token); err != nil {
		t.Fatalf("first approved Run() error=%v", err)
	}
	_, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", input, token)
	var mismatch *ApprovalMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("replayed Run() error=%#v", err)
	}

	var calls int
	if err := a.db.QueryRow(`SELECT count(*) FROM calls`).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("persisted calls=%d err=%v", calls, err)
	}
}

func TestRunConsumesApprovalOnceAcrossConcurrentRequests(t *testing.T) {
	adapter := &asyncTestAdapter{cancelable: true, canceled: make(chan struct{}), requiresApproval: true}
	a, p, _ := setupAsyncApp(t, adapter)
	input := json.RawMessage(`{}`)
	token := approvalForRun(t, a, "owner", p.Config["workspace_root"], "demo/async", input)

	const contenders = 8
	start := make(chan struct{})
	results := make(chan error, contenders)
	var workers sync.WaitGroup
	for range contenders {
		workers.Go(func() {
			<-start
			_, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", input, token)
			results <- err
		})
	}
	close(start)
	workers.Wait()
	close(results)

	succeeded, rejected := 0, 0
	for err := range results {
		var mismatch *ApprovalMismatchError
		switch {
		case err == nil:
			succeeded++
		case errors.As(err, &mismatch):
			rejected++
		default:
			t.Fatalf("concurrent Run() error=%#v", err)
		}
	}
	if succeeded != 1 || rejected != contenders-1 {
		t.Fatalf("concurrent approvals succeeded=%d rejected=%d", succeeded, rejected)
	}

	var calls int
	if err := a.db.QueryRow(`SELECT count(*) FROM calls`).Scan(&calls); err != nil || calls != 1 {
		t.Fatalf("persisted calls=%d err=%v", calls, err)
	}
}

func TestRunRejectsExpiredApproval(t *testing.T) {
	adapter := &asyncTestAdapter{cancelable: true, canceled: make(chan struct{}), requiresApproval: true}
	a, p, _ := setupAsyncApp(t, adapter)
	input := json.RawMessage(`{}`)
	token := approvalForRun(t, a, "owner", p.Config["workspace_root"], "demo/async", input)

	a.approvalMu.Lock()
	challenge := a.approvals[token]
	challenge.expiresAt = time.Now().Add(-time.Second)
	a.approvals[token] = challenge
	a.approvalMu.Unlock()

	_, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", input, token)
	var mismatch *ApprovalMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("expired Run() error=%#v", err)
	}
	var calls int
	if err := a.db.QueryRow(`SELECT count(*) FROM calls`).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("persisted calls=%d err=%v", calls, err)
	}
}

func TestRunRejectsCapabilityChangedBetweenApprovalRequests(t *testing.T) {
	started := make(chan struct{})
	adapter := &asyncTestAdapter{cancelable: true, canceled: make(chan struct{}), started: started, requiresApproval: true}
	a, p, _ := setupAsyncApp(t, adapter)
	input := json.RawMessage(`{}`)
	_, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", input, "")
	var approval *ApprovalRequiredError
	if !errors.As(err, &approval) || approval.Token == "" {
		t.Fatalf("approval preflight error=%#v", err)
	}
	updated, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	updated = scopeIntegrationCapabilities(p, updated)
	if _, err := a.cat.ReplaceProviderCapabilities(context.Background(), p, updated); err != nil {
		t.Fatal(err)
	}

	_, err = a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", input, approval.Token)
	var changed *CapabilityChangedError
	if !errors.As(err, &changed) || changed.Capability != "demo/async" {
		t.Fatalf("approved retry error=%#v", err)
	}
	select {
	case <-started:
		t.Fatal("stale preflight reached the upstream adapter")
	default:
	}
	var calls int
	if err := a.db.QueryRow(`SELECT count(*) FROM calls`).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("stale preflight calls=%d err=%v", calls, err)
	}
}

func TestRunRejectsCapabilityChangedInsideApprovedRequest(t *testing.T) {
	started := make(chan struct{})
	adapter := &asyncTestAdapter{cancelable: true, canceled: make(chan struct{}), started: started, requiresApproval: true}
	a, p, capabilityID := setupAsyncApp(t, adapter)
	stale, err := a.cat.GetCapability(context.Background(), capabilityID)
	if err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{}`)
	token, err := a.issueApproval("owner", stale, input)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	updated = scopeIntegrationCapabilities(p, updated)
	if _, err := a.cat.ReplaceProviderCapabilities(context.Background(), p, updated); err != nil {
		t.Fatal(err)
	}

	_, err = a.runResolved(context.Background(), "owner", "demo/async", input, token, stale)
	var changed *CapabilityChangedError
	if !errors.As(err, &changed) {
		t.Fatalf("runResolved() error=%#v", err)
	}
	select {
	case <-started:
		t.Fatal("stale preflight reached the upstream adapter")
	default:
	}
}

func TestRunRejectsApprovalForDifferentInput(t *testing.T) {
	adapter := &asyncTestAdapter{cancelable: true, canceled: make(chan struct{}), requiresApproval: true}
	a, p, _ := setupAsyncApp(t, adapter)
	_, err := a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", json.RawMessage(`{"value":1}`), "")
	var approval *ApprovalRequiredError
	if !errors.As(err, &approval) {
		t.Fatalf("approval preflight error=%#v", err)
	}
	_, err = a.RunInWorkspace(context.Background(), "owner", p.Config["workspace_root"], "demo/async", json.RawMessage(`{"value":2}`), approval.Token)
	var mismatch *ApprovalMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("changed input error=%#v", err)
	}
}
