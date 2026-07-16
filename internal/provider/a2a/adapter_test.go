package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/jsoncontract"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/secret"
)

type staticResolver map[string]string

func (r staticResolver) Resolve(_ context.Context, reference string) (string, error) {
	value, ok := r[reference]
	if !ok {
		return "", &secret.MissingError{Reference: reference}
	}
	return value, nil
}

func bearerProvider(name, endpoint string) provider.Provider {
	return provider.Provider{
		ID:       "a2a/ws-0000000000000000/" + name,
		Protocol: "a2a",
		Name:     name,
		Endpoint: endpoint,
		Config: map[string]string{
			"workspace_root":       "/tmp/9a-a2a-test",
			"credential_reference": name + ".token",
		},
	}
}

type recordingSink struct {
	mu        sync.Mutex
	order     []string
	events    []provider.Event
	artifacts []string
	started   chan struct{}
}

type blockingResultSink struct {
	recordingSink
	resultEntered chan struct{}
	releaseResult chan struct{}
	resultOnce    sync.Once
}

type blockingArtifactFailureSink struct {
	recordingSink
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	err     error
}

func (s *blockingArtifactFailureSink) Artifact(string, string, []byte) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return s.err
}

func (s *blockingResultSink) Event(event provider.Event) error {
	if event.Type == "result" {
		s.resultOnce.Do(func() { close(s.resultEntered) })
		<-s.releaseResult
	}
	return s.recordingSink.Event(event)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type gatedBody struct {
	wait      <-chan struct{}
	reader    *bytes.Reader
	consumed  chan<- struct{}
	consumeMu sync.Once
}

func (b *gatedBody) Read(data []byte) (int, error) {
	<-b.wait
	n, err := b.reader.Read(data)
	if errors.Is(err, io.EOF) && b.consumed != nil {
		b.consumeMu.Do(func() { close(b.consumed) })
	}
	return n, err
}
func (*gatedBody) Close() error { return nil }

func (s *recordingSink) Started() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "started")
	if s.started != nil {
		close(s.started)
		s.started = nil
	}
	return nil
}

func TestCancelUsesInvocationIDAndConfirmsRemoteCancellation(t *testing.T) {
	cancelRequests := make(chan map[string]any, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/a2a+json")
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(validCard(server.URL))
		case "/a2a/v1/message:send":
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{"id": "remote/task 1", "status": map[string]any{"state": "TASK_STATE_SUBMITTED"}}})
		case "/a2a/v1/tasks/remote%2Ftask%201:cancel":
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			cancelRequests <- request
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "remote/task 1", "status": map[string]any{"state": "TASK_STATE_CANCELED"}})
		case "/a2a/v1/tasks/remote%2Ftask%201":
			<-r.Context().Done()
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := New()
	adapter.pollInterval = time.Millisecond
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	sink := &recordingSink{started: started}
	invokeDone := make(chan error, 1)
	go func() {
		invokeDone <- adapter.Invoke(context.Background(), p, capabilities[0], "call-to-cancel", json.RawMessage(`{"parts":[{"text":"work"}]}`), sink)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("Invoke did not start")
	}
	if err := adapter.Cancel(context.Background(), p, "call-to-cancel"); err != nil {
		t.Fatal(err)
	}
	if request := <-cancelRequests; request["tenant"] != "tenant-a" {
		t.Fatalf("cancel request=%#v", request)
	}
	select {
	case err := <-invokeDone:
		var adapterErr *provider.AdapterError
		if !errors.As(err, &adapterErr) || adapterErr.Code() != "canceled" {
			t.Fatalf("Invoke error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Invoke did not stop after cancellation")
	}
	if err := adapter.Cancel(context.Background(), p, "remote/task 1"); err == nil {
		t.Fatal("Cancel accepted a remote task ID")
	}
}

func TestCompletionClaimsTerminalBeforeBlockedResultSink(t *testing.T) {
	cancelRequests := make(chan struct{}, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/a2a+json")
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(validCard(server.URL))
		case "/a2a/v1/message:send":
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{"id": "terminal-task", "contextId": "terminal-context", "status": map[string]any{"state": "TASK_STATE_SUBMITTED"}}})
		case "/a2a/v1/tasks/terminal-task":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "terminal-task", "contextId": "terminal-context", "status": map[string]any{"state": "TASK_STATE_COMPLETED"}})
		case "/a2a/v1/tasks/terminal-task:cancel":
			cancelRequests <- struct{}{}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "terminal-task", "contextId": "terminal-context", "status": map[string]any{"state": "TASK_STATE_CANCELED"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := New()
	adapter.pollInterval = time.Millisecond
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	sink := &blockingResultSink{resultEntered: make(chan struct{}), releaseResult: make(chan struct{})}
	invokeDone := make(chan error, 1)
	go func() {
		invokeDone <- adapter.Invoke(context.Background(), p, capabilities[0], "terminal-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), sink)
	}()
	<-sink.resultEntered
	err = adapter.Cancel(context.Background(), p, "terminal-call")
	var adapterErr *provider.AdapterError
	cancelWasNotActive := errors.As(err, &adapterErr) && adapterErr.Code() == "not_cancelable"
	cancelRequestCount := len(cancelRequests)
	close(sink.releaseResult)
	if err := <-invokeDone; err != nil {
		t.Fatalf("Invoke error=%v", err)
	}
	if !cancelWasNotActive {
		t.Fatalf("Cancel error=%v", err)
	}
	if cancelRequestCount != 0 {
		t.Fatal("Cancel sent HTTP after completion claimed terminal ownership")
	}
}

func TestCancelClaimsTerminalWhilePollReceivesCompleted(t *testing.T) {
	cancelStarted := make(chan struct{})
	pollStarted := make(chan struct{})
	pollConsumed := make(chan struct{})
	var pollOnce, cancelOnce sync.Once
	response := func(body io.ReadCloser) *http.Response {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: body}
	}
	adapter := New()
	adapter.pollInterval = time.Millisecond
	adapter.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		switch request.URL.EscapedPath() {
		case "/a2a/v1/message:send":
			return response(io.NopCloser(strings.NewReader(`{"task":{"id":"owned-task","contextId":"owned-context","status":{"state":"TASK_STATE_SUBMITTED"}}}`))), nil
		case "/a2a/v1/tasks/owned-task":
			pollOnce.Do(func() { close(pollStarted) })
			return response(&gatedBody{wait: cancelStarted, reader: bytes.NewReader([]byte(`{"id":"owned-task","contextId":"owned-context","status":{"state":"TASK_STATE_COMPLETED"}}`)), consumed: pollConsumed}), nil
		case "/a2a/v1/tasks/owned-task:cancel":
			cancelOnce.Do(func() { close(cancelStarted) })
			return response(&gatedBody{wait: pollConsumed, reader: bytes.NewReader([]byte(`{"id":"owned-task","contextId":"owned-context","status":{"state":"TASK_STATE_CANCELED"}}`))}), nil
		default:
			return nil, errors.New("unexpected A2A request")
		}
	})}
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: "http://agent.example"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://agent.example/a2a/v1", tenant: "tenant"}
	invokeDone := make(chan error, 1)
	go func() {
		invokeDone <- adapter.Invoke(context.Background(), p, capability.Capability{}, "owned-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
	}()
	<-pollStarted
	cancelDone := make(chan error, 1)
	go func() { cancelDone <- adapter.Cancel(context.Background(), p, "owned-call") }()
	if err := <-cancelDone; err != nil {
		t.Fatalf("Cancel error=%v", err)
	}
	err := <-invokeDone
	var adapterErr *provider.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code() != "canceled" {
		t.Fatalf("Invoke error=%v", err)
	}
}

func TestCancelAndLocalSinkFailureHaveSingleTerminalOwner(t *testing.T) {
	for _, cancelWins := range []bool{true, false} {
		t.Run(fmt.Sprintf("cancel_wins_%v", cancelWins), func(t *testing.T) {
			cancelRequests := make(chan struct{}, 1)
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/a2a+json")
				switch r.URL.EscapedPath() {
				case "/.well-known/agent-card.json":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(validCard(server.URL))
				case "/a2a/v1/message:send":
					artifact := map[string]any{"artifactId": "artifact", "parts": []any{map[string]any{"text": "data"}}}
					task := map[string]any{"id": "sink-task", "status": map[string]any{"state": "TASK_STATE_SUBMITTED"}, "artifacts": []any{artifact}}
					_ = json.NewEncoder(w).Encode(map[string]any{"task": task})
				case "/a2a/v1/tasks/sink-task:cancel":
					cancelRequests <- struct{}{}
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "sink-task", "status": map[string]any{"state": "TASK_STATE_CANCELED"}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			adapter := New()
			p := provider.Provider{ID: "a2a/ws-0000000000000000/public-agent", Protocol: "a2a", Name: "public-agent", Endpoint: server.URL}
			capabilities, err := adapter.Discover(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			sinkErr := errors.New("local sink failure")
			sink := &blockingArtifactFailureSink{entered: make(chan struct{}), release: make(chan struct{}), err: sinkErr}
			invokeDone := make(chan error, 1)
			go func() {
				invokeDone <- adapter.Invoke(context.Background(), p, capabilities[0], "sink-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), sink)
			}()
			<-sink.entered
			if cancelWins {
				cancelDone := make(chan error, 1)
				go func() { cancelDone <- adapter.Cancel(context.Background(), p, "sink-call") }()
				<-cancelRequests
				if err := <-cancelDone; err != nil {
					t.Fatal(err)
				}
				close(sink.release)
				err = <-invokeDone
				var adapterErr *provider.AdapterError
				if !errors.As(err, &adapterErr) || adapterErr.Code() != "canceled" {
					t.Fatalf("Invoke error=%v", err)
				}
			} else {
				close(sink.release)
				err = <-invokeDone
				if !errors.Is(err, sinkErr) {
					t.Fatalf("Invoke error=%v", err)
				}
				err = adapter.Cancel(context.Background(), p, "sink-call")
				var adapterErr *provider.AdapterError
				if !errors.As(err, &adapterErr) || adapterErr.Code() != "not_cancelable" || len(cancelRequests) != 0 {
					t.Fatalf("Cancel error=%v requests=%d", err, len(cancelRequests))
				}
			}
		})
	}
}

func TestFinishTaskExitArbitratesRepresentativeLocalFailures(t *testing.T) {
	localErrors := []error{
		adapterError("a2a_timeout", "A2A task lifetime exceeded"),
		adapterError("a2a_unavailable", "A2A operation endpoint is unavailable"),
		adapterError("invalid_response", "invalid A2A Task response"),
		context.Canceled,
		errors.New("sink failure"),
	}
	for i, localErr := range localErrors {
		t.Run(fmt.Sprintf("local_claim_%d", i), func(t *testing.T) {
			adapter := New()
			active := &activeTask{updates: make(chan json.RawMessage, 1)}
			adapter.active["call"] = active
			if got := adapter.finishTaskExit("call", active, localErr); !errors.Is(got, localErr) {
				t.Fatalf("finishTaskExit()=%v want %v", got, localErr)
			}
			if len(adapter.active) != 0 || active.owner != terminalPoll {
				t.Fatalf("active=%#v owner=%v", adapter.active, active.owner)
			}
		})
		t.Run(fmt.Sprintf("cancel_claim_%d", i), func(t *testing.T) {
			adapter := New()
			active := &activeTask{owner: terminalCancel, updates: make(chan json.RawMessage, 1)}
			active.updates <- json.RawMessage(`{"id":"task","status":{"state":"TASK_STATE_CANCELED"}}`)
			got := adapter.finishTaskExit("call", active, localErr)
			var adapterErr *provider.AdapterError
			if !errors.As(got, &adapterErr) || adapterErr.Code() != "canceled" {
				t.Fatalf("finishTaskExit()=%v", got)
			}
		})
	}
}

func TestCancelDeliveryFailureKeepsInvocationActive(t *testing.T) {
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: "http://agent.example"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://agent.example/a2a/v1", tenant: "tenant"}
	updates := make(chan json.RawMessage, 1)
	updates <- json.RawMessage(`{"occupied":true}`)
	active := &activeTask{providerID: p.ID, taskID: "task", tenant: "tenant", cancel: func() {}, updates: updates}
	adapter.active["call"] = active
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"task","status":{"state":"TASK_STATE_CANCELED"}}`))}, nil
	})}
	if err := adapter.Cancel(context.Background(), p, "call"); err == nil {
		t.Fatal("Cancel accepted an undeliverable terminal update")
	}
	adapter.mu.Lock()
	attached := adapter.active["call"] == active
	adapter.mu.Unlock()
	if !attached {
		t.Fatal("Cancel detached invocation after local delivery failure")
	}
}

func TestUpstreamCancelFailureKeepsInvocationActiveAndRetryable(t *testing.T) {
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: "http://agent.example"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://agent.example/a2a/v1"}
	active := &activeTask{providerID: p.ID, taskID: "task", tenant: "tenant", cancel: func() {}, updates: make(chan json.RawMessage, 1)}
	adapter.active["call"] = active
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(`{"error":"unavailable"}`))}, nil
	})}
	if err := adapter.Cancel(context.Background(), p, "call"); err == nil {
		t.Fatal("Cancel accepted upstream failure")
	}
	adapter.mu.Lock()
	attached := adapter.active["call"] == active
	canceling := active.canceling
	adapter.mu.Unlock()
	if !attached || canceling {
		t.Fatalf("attached=%v canceling=%v", attached, canceling)
	}
}

func TestActiveRegistryClearsForDirectAndEveryTerminalTaskState(t *testing.T) {
	states := []string{"TASK_STATE_COMPLETED", "TASK_STATE_FAILED", "TASK_STATE_CANCELED", "TASK_STATE_INPUT_REQUIRED", "TASK_STATE_REJECTED", "TASK_STATE_AUTH_REQUIRED", "TASK_STATE_UNSPECIFIED"}
	for _, state := range states {
		t.Run(state, func(t *testing.T) {
			adapter := New()
			p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: "http://agent.example"}
			adapter.cache[p.ID] = resolvedProvider{baseURL: "http://agent.example/a2a/v1"}
			body := `{"task":{"id":"terminal","contextId":"context","status":{"state":"` + state + `"}}}`
			adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			err := adapter.Invoke(context.Background(), p, capability.Capability{}, "terminal-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
			if state == "TASK_STATE_COMPLETED" && err != nil || state != "TASK_STATE_COMPLETED" && err == nil {
				t.Fatalf("Invoke error=%v", err)
			}
			adapter.mu.Lock()
			activeCount := len(adapter.active)
			adapter.mu.Unlock()
			if activeCount != 0 {
				t.Fatalf("active count=%d", activeCount)
			}
		})
	}
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: "http://agent.example"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://agent.example/a2a/v1"}
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(`{"message":{"messageId":"direct","contextId":"context","role":"ROLE_AGENT","parts":[{"text":"done"}]}}`))}, nil
	})}
	if err := adapter.Invoke(context.Background(), p, capability.Capability{}, "direct-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{}); err != nil {
		t.Fatal(err)
	}
	if len(adapter.active) != 0 {
		t.Fatalf("direct active=%#v", adapter.active)
	}
}

func TestCloseCancelsAndClearsActiveTask(t *testing.T) {
	pollStarted := make(chan struct{})
	var pollOnce sync.Once
	adapter := New()
	adapter.pollInterval = time.Millisecond
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: "http://agent.example"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://agent.example/a2a/v1"}
	adapter.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method == http.MethodPost {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(`{"task":{"id":"close-task","status":{"state":"TASK_STATE_SUBMITTED"}}}`))}, nil
		}
		pollOnce.Do(func() { close(pollStarted) })
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	invokeDone := make(chan error, 1)
	go func() {
		invokeDone <- adapter.Invoke(context.Background(), p, capability.Capability{}, "close-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
	}()
	<-pollStarted
	if err := adapter.Close(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if err := <-invokeDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke error=%v", err)
	}
	adapter.mu.Lock()
	activeCount := len(adapter.active)
	adapter.mu.Unlock()
	if activeCount != 0 {
		t.Fatalf("active count=%d", activeCount)
	}
}

func TestSixtyFifthConcurrentInvokeFailsBeforeOperationHTTP(t *testing.T) {
	const limit = 64
	release := make(chan struct{})
	started := make(chan struct{}, limit+1)
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/public-agent", Protocol: "a2a", Name: "public-agent", Endpoint: "http://127.0.0.1"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://127.0.0.1/a2a/v1"}
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-release
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(`{"message":{"messageId":"direct","contextId":"context","role":"ROLE_AGENT","parts":[{"text":"done"}]}}`))}, nil
	})}
	done := make(chan error, limit)
	for i := 0; i < limit; i++ {
		go func(i int) {
			done <- adapter.Invoke(context.Background(), p, capability.Capability{}, fmt.Sprintf("call-%d", i), json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
		}(i)
	}
	for i := 0; i < limit; i++ {
		<-started
	}
	overflow := make(chan error, 1)
	go func() {
		overflow <- adapter.Invoke(context.Background(), p, capability.Capability{}, "overflow", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
	}()
	var overflowErr error
	select {
	case overflowErr = <-overflow:
	case <-time.After(time.Second):
		overflowErr = errors.New("overflow invocation reached or blocked in HTTP")
	}
	if len(started) != 0 {
		overflowErr = errors.New("overflow invocation reached operation HTTP")
	}
	releaseOnce.Do(func() { close(release) })
	for i := 0; i < limit; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Invoke %d error=%v", i, err)
		}
	}
	var adapterErr *provider.AdapterError
	if !errors.As(overflowErr, &adapterErr) || adapterErr.Code() != "resource_exhausted" {
		t.Fatalf("overflow error=%v", overflowErr)
	}
}

func TestSixtyFifthConcurrentInvokeFailsBeforeAgentCardHTTP(t *testing.T) {
	const limit = 64
	release := make(chan struct{})
	cardStarted := make(chan struct{}, limit+1)
	var releaseOnce sync.Once
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			cardStarted <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(validCard(server.URL))
		case "/a2a/v1/message:send":
			w.Header().Set("Content-Type", "application/a2a+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"messageId": "direct", "contextId": "context", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "done"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		releaseOnce.Do(func() { close(release) })
		server.Close()
	}()

	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/public-agent", Protocol: "a2a", Name: "public-agent", Endpoint: server.URL}
	done := make(chan error, limit)
	for i := 0; i < limit; i++ {
		go func(i int) {
			done <- adapter.Invoke(context.Background(), p, capability.Capability{}, fmt.Sprintf("discover-call-%d", i), json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
		}(i)
	}
	for i := 0; i < limit; i++ {
		select {
		case <-cardStarted:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d discovery requests started", i)
		}
	}

	overflow := make(chan error, 1)
	go func() {
		overflow <- adapter.Invoke(context.Background(), p, capability.Capability{}, "discover-overflow", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
	}()
	var overflowErr error
	reachedDiscovery := false
	select {
	case overflowErr = <-overflow:
	case <-cardStarted:
		reachedDiscovery = true
	case <-time.After(time.Second):
		overflowErr = errors.New("overflow invocation neither failed nor reached discovery")
	}

	releaseOnce.Do(func() { close(release) })
	for i := 0; i < limit; i++ {
		if err := <-done; err != nil {
			t.Fatalf("Invoke %d error=%v", i, err)
		}
	}
	if reachedDiscovery {
		overflowErr = <-overflow
		t.Fatalf("overflow reached Agent Card HTTP and returned %v", overflowErr)
	}
	var adapterErr *provider.AdapterError
	if !errors.As(overflowErr, &adapterErr) || adapterErr.Code() != "resource_exhausted" {
		t.Fatalf("overflow error=%v", overflowErr)
	}
}

func TestInvokeResolutionAuthAndInterfaceFailuresReleaseQuota(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any) map[string]any
	}{
		{name: "invalid card", mutate: func(map[string]any) map[string]any { return map[string]any{} }},
		{name: "missing auth", mutate: func(card map[string]any) map[string]any {
			card["securitySchemes"] = map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}
			card["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{}}}}
			return card
		}},
		{name: "unsupported interface", mutate: func(card map[string]any) map[string]any {
			card["supportedInterfaces"].([]any)[1].(map[string]any)["protocolVersion"] = "2.0"
			return card
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var mu sync.Mutex
			discoveries := 0
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.EscapedPath() {
				case "/.well-known/agent-card.json":
					mu.Lock()
					discoveries++
					attempt := discoveries
					mu.Unlock()
					card := validCard(server.URL)
					if attempt == 1 {
						card = test.mutate(card)
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(card)
				case "/a2a/v1/message:send":
					w.Header().Set("Content-Type", "application/a2a+json")
					_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"messageId": "direct", "contextId": "context", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "done"}}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()

			adapter := New()
			adapter.maxActiveInvocations = 1
			p := provider.Provider{ID: "a2a/ws-0000000000000000/retry-agent", Protocol: "a2a", Name: "retry-agent", Endpoint: server.URL}
			input := json.RawMessage(`{"parts":[{"text":"work"}]}`)
			if err := adapter.Invoke(context.Background(), p, capability.Capability{}, "failed-resolution", input, &recordingSink{}); err == nil {
				t.Fatal("Invoke accepted invalid Agent Card")
			}
			if err := adapter.Invoke(context.Background(), p, capability.Capability{}, "successful-retry", input, &recordingSink{}); err != nil {
				t.Fatalf("Invoke after resolution failure: %v", err)
			}
		})
	}
}

func TestTaskLifetimeExpiresWithTypedTimeoutAndReleasesResources(t *testing.T) {
	adapter := New()
	adapter.pollInterval = time.Millisecond
	adapter.taskTimeout = 15 * time.Millisecond
	p := provider.Provider{ID: "a2a/ws-0000000000000000/public-agent", Protocol: "a2a", Name: "public-agent", Endpoint: "http://127.0.0.1"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://127.0.0.1/a2a/v1"}
	adapter.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"task":{"id":"timeout-task","status":{"state":"TASK_STATE_SUBMITTED"}}}`
		if request.Method == http.MethodGet {
			body = `{"id":"timeout-task","status":{"state":"TASK_STATE_WORKING"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := adapter.Invoke(ctx, p, capability.Capability{}, "timeout-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
	var adapterErr *provider.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code() != "a2a_timeout" {
		t.Fatalf("Invoke error=%v", err)
	}
	adapter.mu.Lock()
	activeTasks, activeInvocations := len(adapter.active), adapter.activeInvocations
	adapter.mu.Unlock()
	if activeTasks != 0 || activeInvocations != 0 {
		t.Fatalf("active tasks=%d invocations=%d", activeTasks, activeInvocations)
	}
}

func TestPollingBackoffDoublesWhenUnchangedAndResetsOnChange(t *testing.T) {
	adapter := New()
	adapter.pollInterval = 5 * time.Millisecond
	adapter.maxPollInterval = 20 * time.Millisecond
	adapter.taskTimeout = time.Second
	p := provider.Provider{ID: "a2a/ws-0000000000000000/public-agent", Protocol: "a2a", Name: "public-agent", Endpoint: "http://127.0.0.1"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://127.0.0.1/a2a/v1"}
	var mu sync.Mutex
	var polls []time.Time
	adapter.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"task":{"id":"backoff-task","status":{"state":"TASK_STATE_SUBMITTED"}}}`
		if request.Method == http.MethodGet {
			mu.Lock()
			polls = append(polls, time.Now())
			count := len(polls)
			mu.Unlock()
			state := "TASK_STATE_WORKING"
			if count >= 4 {
				state = "TASK_STATE_COMPLETED"
			}
			body = `{"id":"backoff-task","status":{"state":"` + state + `"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	if err := adapter.Invoke(context.Background(), p, capability.Capability{}, "backoff-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	observed := append([]time.Time(nil), polls...)
	mu.Unlock()
	if len(observed) != 4 {
		t.Fatalf("poll count=%d", len(observed))
	}
	wantMinimums := []time.Duration{4 * time.Millisecond, 8 * time.Millisecond, 16 * time.Millisecond}
	for i, minimum := range wantMinimums {
		if gap := observed[i+1].Sub(observed[i]); gap < minimum {
			t.Fatalf("poll gap %d=%v want >=%v", i+1, gap, minimum)
		}
	}
}

func TestExplicitCancelInterruptsPollingBackoff(t *testing.T) {
	adapter := New()
	adapter.pollInterval = 20 * time.Millisecond
	adapter.maxPollInterval = 200 * time.Millisecond
	adapter.taskTimeout = time.Second
	p := provider.Provider{ID: "a2a/ws-0000000000000000/public-agent", Protocol: "a2a", Name: "public-agent", Endpoint: "http://127.0.0.1"}
	adapter.cache[p.ID] = resolvedProvider{baseURL: "http://127.0.0.1/a2a/v1"}
	secondPoll := make(chan struct{})
	var polls int
	adapter.client = &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		body := `{"task":{"id":"cancel-backoff","status":{"state":"TASK_STATE_SUBMITTED"}}}`
		if request.Method == http.MethodGet {
			polls++
			if polls == 2 {
				close(secondPoll)
			}
			body = `{"id":"cancel-backoff","status":{"state":"TASK_STATE_WORKING"}}`
		} else if strings.HasSuffix(request.URL.Path, ":cancel") {
			body = `{"id":"cancel-backoff","status":{"state":"TASK_STATE_CANCELED"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	invokeDone := make(chan error, 1)
	go func() {
		invokeDone <- adapter.Invoke(context.Background(), p, capability.Capability{}, "cancel-backoff-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
	}()
	<-secondPoll
	started := time.Now()
	if err := adapter.Cancel(context.Background(), p, "cancel-backoff-call"); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed >= 100*time.Millisecond {
		t.Fatalf("Cancel took %v during polling backoff", elapsed)
	}
	err := <-invokeDone
	var adapterErr *provider.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code() != "canceled" {
		t.Fatalf("Invoke error=%v", err)
	}
}
func (s *recordingSink) Event(event provider.Event) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "event:"+event.Type)
	s.events = append(s.events, event)
	return nil
}
func (s *recordingSink) Artifact(name, mediaType string, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append(s.order, "artifact:"+name)
	s.artifacts = append(s.artifacts, mediaType+":"+string(data))
	return nil
}

func validCard(restURL string) map[string]any {
	return map[string]any{
		"name":        "Research Agent",
		"description": "Researches and summarizes supplied material.",
		"supportedInterfaces": []any{
			map[string]any{"url": restURL + "/rpc", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"},
			map[string]any{"url": restURL + "/a2a/v1", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0", "tenant": "tenant-a"},
		},
		"version":            "1.0.0",
		"capabilities":       map[string]any{"streaming": false},
		"defaultInputModes":  []string{"text/plain"},
		"defaultOutputModes": []string{"application/json"},
		"skills": []any{
			map[string]any{
				"id": "summarize", "name": "Summarize", "description": "Summarize supplied material.",
				"tags": []string{"research", "summary"}, "examples": []string{"Summarize this report"},
				"inputModes": []string{"text/plain"}, "outputModes": []string{"application/json"},
			},
		},
	}
}

func bearerCard(restURL string) map[string]any {
	card := validCard(restURL)
	card["securitySchemes"] = map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}
	card["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"list": []any{}}}}}
	return card
}

func TestDiscoverSelectsHTTPJSON10AndMapsSkills(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/agent-card.json" {
			t.Errorf("discovery path=%q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("A2A-Version"); got != "1.0" {
			t.Errorf("A2A-Version=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(validCard(server.URL))
	}))
	defer server.Close()

	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 {
		t.Fatalf("capabilities=%#v", capabilities)
	}
	c := capabilities[0]
	if c.ID != "a2a/research-agent/summarize" || c.Kind != "a2a.skill" || c.Source.UpstreamName != "summarize" {
		t.Fatalf("capability identity=%#v", c)
	}
	if c.Input.Mode != "json" || c.Output.Mode != "a2a.response" || !c.Lifecycle.Sync || c.Lifecycle.MultiTurn || !c.Lifecycle.Cancelable || c.Lifecycle.Streaming {
		t.Fatalf("capability contract/lifecycle=%#v", c)
	}
	if c.Output.JSONSchema == nil {
		t.Fatal("A2A output contract did not publish an explicit schema object")
	}
	if c.Security.RequiresApproval != "always" || c.Security.UpstreamAuth != "none" || len(c.RawMetadata) == 0 {
		t.Fatalf("capability security/metadata=%#v", c)
	}
	if err := jsoncontract.Validate(c.Input.JSONSchema, json.RawMessage(`{"parts":[{"text":"summarize"}]}`)); err != nil {
		t.Fatalf("published input schema rejected a valid A2A request: %v", err)
	}
	if err := jsoncontract.Validate(c.Input.JSONSchema, json.RawMessage(`{"parts":[{}]}`)); !errors.Is(err, jsoncontract.ErrInvalidValue) {
		t.Fatalf("published input schema accepted an empty A2A part: %v", err)
	}
}

func TestDiscoverRejectsNonCanonicalProviderNameBeforeHTTP(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests++ }))
	defer server.Close()
	adapter := New()
	_, err := adapter.Discover(context.Background(), provider.Provider{ID: "a2a/ws-0000000000000000/Bad_Name", Protocol: "a2a", Name: "Bad_Name", Endpoint: server.URL})
	if err == nil || requests != 0 {
		t.Fatalf("Discover error=%v requests=%d", err, requests)
	}
}

func TestDiscoverRejectsNonLoopbackCleartextBeforeHTTP(t *testing.T) {
	requests := 0
	adapter := New()
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("must not be called")
	})}
	p := provider.Provider{ID: "a2a/ws-0000000000000000/public-agent", Protocol: "a2a", Name: "public-agent", Endpoint: "http://agent.example"}
	if _, err := adapter.Discover(context.Background(), p); err == nil || requests != 0 {
		t.Fatalf("Discover error=%v requests=%d", err, requests)
	}
}

func TestDiscoverRejectsCrossOriginInterfaceBeforeCacheOrOperation(t *testing.T) {
	attackerRequests := 0
	attacker := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		attackerRequests++
		if r.Header.Get("Authorization") != "" {
			t.Error("token leaked cross-origin")
		}
	}))
	defer attacker.Close()
	var cardServer *httptest.Server
	cardServer = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		card := bearerCard(cardServer.URL)
		card["supportedInterfaces"] = []any{map[string]any{"url": attacker.URL + "/a2a/v1", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0"}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}))
	defer cardServer.Close()
	adapter := New()
	p := bearerProvider("research-agent", cardServer.URL)
	capabilities, err := adapter.Discover(context.Background(), p)
	if err == nil || capabilities != nil || attackerRequests != 0 {
		t.Fatalf("Discover capabilities=%#v error=%v attackerRequests=%d", capabilities, err, attackerRequests)
	}
	adapter.mu.Lock()
	_, cached := adapter.cache[p.ID]
	adapter.mu.Unlock()
	if cached {
		t.Fatal("cross-origin interface was cached")
	}
}

func TestDiscoverRejectsOversizedInterfaceURL(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		card := validCard(server.URL)
		card["supportedInterfaces"] = []any{map[string]any{"url": server.URL + "/" + strings.Repeat("x", maxStringBytes), "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0"}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}))
	defer server.Close()
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	if _, err := adapter.Discover(context.Background(), p); err == nil {
		t.Fatal("Discover accepted oversized interface URL")
	}
}

func TestPublicCardNeverReceivesAccidentalProviderToken(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(validCard(server.URL))
		case "/a2a/v1/message:send":
			if r.Header.Get("Authorization") != "" {
				t.Errorf("public operation Authorization=%q", r.Header.Get("Authorization"))
			}
			w.Header().Set("Content-Type", "application/a2a+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"messageId": "response", "contextId": "context", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "done"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := NewWithResolver(staticResolver{"public-agent.token": "accidental-secret"})
	p := bearerProvider("public-agent", server.URL)
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Invoke(context.Background(), p, capabilities[0], "public-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{}); err != nil {
		t.Fatal(err)
	}
}

func TestDiscoverRejectsSkillIDWithoutPublicSlug(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		card := validCard(server.URL)
		card["skills"].([]any)[0].(map[string]any)["id"] = "!!!"
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}))
	defer server.Close()
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/public-agent", Protocol: "a2a", Name: "public-agent", Endpoint: server.URL}
	if capabilities, err := adapter.Discover(context.Background(), p); err == nil || capabilities != nil {
		t.Fatalf("Discover capabilities=%#v error=%v", capabilities, err)
	}
}

func TestBearerCardRequiresProviderTokenAndRejectsUnsupportedAuth(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "missing bearer token", mutate: func(map[string]any) {}},
		{name: "unsupported api key", mutate: func(card map[string]any) {
			card["securitySchemes"] = map[string]any{"api": map[string]any{"apiKeySecurityScheme": map[string]any{"location": "header", "name": "X-Key"}}}
			card["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{"api": map[string]any{"list": []any{}}}}}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("private card discovery received Authorization")
				}
				card := bearerCard(server.URL)
				test.mutate(card)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(card)
			}))
			defer server.Close()
			adapter := New()
			p := provider.Provider{ID: "a2a/ws-0000000000000000/private-agent", Protocol: "a2a", Name: "private-agent", Endpoint: server.URL}
			if _, err := adapter.Discover(context.Background(), p); err == nil || strings.Contains(err.Error(), "private-agent") {
				t.Fatalf("Discover error=%v", err)
			}
		})
	}
}

func TestSecurityRequirementAlternativesPreferExplicitPublicAccessAndIsolateProviders(t *testing.T) {
	cardMap := bearerCard("https://agent.example")
	cardMap["securityRequirements"] = []any{
		map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"list": []any{}}}},
		map[string]any{"schemes": map[string]any{}},
	}
	encoded, _ := json.Marshal(cardMap)
	var card agentCard
	if err := json.Unmarshal(encoded, &card); err != nil {
		t.Fatal(err)
	}
	if bearer, err := cardBearerPolicy(card, "public-choice"); err != nil || bearer {
		t.Fatalf("public alternative bearer=%v error=%v", bearer, err)
	}
	card.SecurityRequirements = card.SecurityRequirements[:1]
	if bearer, err := cardBearerPolicy(card, "first-agent"); err != nil || !bearer {
		t.Fatalf("first provider bearer=%v error=%v", bearer, err)
	}
	if bearer, err := cardBearerPolicy(card, "second-agent"); err != nil || !bearer {
		t.Fatalf("second provider bearer=%v error=%v", bearer, err)
	}
}

func TestBearerCardAcceptsCanonicalEmptyStringList(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			card := bearerCard(server.URL)
			card["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{}}}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(card)
		case "/a2a/v1/message:send":
			if got := r.Header.Get("Authorization"); got != "Bearer canonical-secret" {
				t.Errorf("Authorization=%q", got)
			}
			w.Header().Set("Content-Type", "application/a2a+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"messageId": "response", "contextId": "context", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "done"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := NewWithResolver(staticResolver{"canonical-agent.token": "canonical-secret"})
	p := bearerProvider("canonical-agent", server.URL)
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if err := adapter.Invoke(context.Background(), p, capabilities[0], "canonical-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{}); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityRequirementsRejectMalformedStringListsScopesAndAND(t *testing.T) {
	for _, test := range []struct {
		name         string
		requirements []any
		schemes      map[string]any
	}{
		{name: "null string list", requirements: []any{map[string]any{"schemes": map[string]any{"bearerAuth": nil}}}, schemes: map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}},
		{name: "null list field", requirements: []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"list": nil}}}}, schemes: map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}},
		{name: "string list field", requirements: []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"list": "scope"}}}}, schemes: map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}},
		{name: "object list field", requirements: []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"list": map[string]any{}}}}}, schemes: map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}},
		{name: "nonstring list item", requirements: []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"list": []any{1}}}}}, schemes: map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}},
		{name: "unknown string list field", requirements: []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"unknown": []any{}}}}}, schemes: map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}},
		{name: "array string list", requirements: []any{map[string]any{"schemes": map[string]any{"bearerAuth": []any{}}}}, schemes: map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}},
		{name: "nonempty bearer scopes", requirements: []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"list": []any{"scope.read"}}}}}, schemes: map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}},
		{name: "missing scheme reference", requirements: []any{map[string]any{"schemes": map[string]any{"missing": map[string]any{"list": []any{}}}}}, schemes: map[string]any{}},
		{name: "multi bearer AND", requirements: []any{map[string]any{"schemes": map[string]any{"first": map[string]any{"list": []any{}}, "second": map[string]any{"list": []any{}}}}}, schemes: map[string]any{
			"first": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}, "second": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}},
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				card := validCard(server.URL)
				card["securitySchemes"] = test.schemes
				card["securityRequirements"] = test.requirements
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(card)
			}))
			defer server.Close()
			adapter := New()
			p := provider.Provider{ID: "a2a/ws-0000000000000000/private-agent", Protocol: "a2a", Name: "private-agent", Endpoint: server.URL}
			if _, err := adapter.Discover(context.Background(), p); err == nil {
				t.Fatal("Discover accepted invalid SecurityRequirement")
			}
		})
	}
}

func TestSkillSecurityRequirementsOverrideCardPolicy(t *testing.T) {
	for _, test := range []struct {
		name           string
		cardBearer     bool
		skillBearer    bool
		setToken       bool
		wantAuthHeader bool
	}{
		{name: "skill public overrides card bearer", cardBearer: true},
		{name: "skill bearer overrides card public", skillBearer: true, setToken: true, wantAuthHeader: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.EscapedPath() {
				case "/.well-known/agent-card.json":
					card := validCard(server.URL)
					if test.cardBearer {
						card = bearerCard(server.URL)
					}
					skills := card["skills"].([]any)
					skill := skills[0].(map[string]any)
					if test.skillBearer {
						card["securitySchemes"] = map[string]any{"skillBearer": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}
						skill["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{"skillBearer": map[string]any{"list": []any{}}}}}
					} else {
						skill["securityRequirements"] = []any{map[string]any{"schemes": map[string]any{}}}
					}
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(card)
				case "/a2a/v1/message:send":
					gotAuth := r.Header.Get("Authorization")
					if (gotAuth != "") != test.wantAuthHeader {
						t.Errorf("Authorization=%q", gotAuth)
					}
					w.Header().Set("Content-Type", "application/a2a+json")
					_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"messageId": "response", "contextId": "context", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "done"}}}})
				default:
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			adapter := NewWithResolver(staticResolver{"skill-agent.token": "skill-secret"})
			p := provider.Provider{ID: "a2a/ws-0000000000000000/skill-agent", Protocol: "a2a", Name: "skill-agent", Endpoint: server.URL}
			if test.setToken {
				p = bearerProvider("skill-agent", server.URL)
			}
			capabilities, err := adapter.Discover(context.Background(), p)
			if err != nil {
				t.Fatal(err)
			}
			if err := adapter.Invoke(context.Background(), p, capabilities[0], "skill-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMixedSkillCardRejectsProviderWhenBearerSkillTokenMissing(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		card := validCard(server.URL)
		card["securitySchemes"] = map[string]any{"bearer": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}}
		card["skills"] = []any{
			map[string]any{"id": "public", "name": "Public", "description": "Public skill.", "tags": []string{"public"}, "securityRequirements": []any{map[string]any{"schemes": map[string]any{}}}},
			map[string]any{
				"id": "private", "name": "Private", "description": "Private skill.", "tags": []string{"private"},
				"securityRequirements": []any{map[string]any{"schemes": map[string]any{"bearer": map[string]any{"list": []any{}}}}},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}))
	defer server.Close()
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/mixed-agent", Protocol: "a2a", Name: "mixed-agent", Endpoint: server.URL}
	if capabilities, err := adapter.Discover(context.Background(), p); err == nil || capabilities != nil {
		t.Fatalf("Discover capabilities=%#v error=%v", capabilities, err)
	}
}

func TestDiscoveryAndOperationErrorsAreSanitizedAdapterErrors(t *testing.T) {
	const sentinel = "https://secret.internal token=leak tenant=private dial tcp 10.0.0.1"
	assertSafe := func(t *testing.T, err error) {
		t.Helper()
		var adapterErr *provider.AdapterError
		if !errors.As(err, &adapterErr) || adapterErr == nil || !adapterErr.Valid() {
			t.Fatalf("error is not a valid AdapterError: %T %v", err, err)
		}
		if strings.Contains(err.Error(), "secret.internal") || strings.Contains(err.Error(), "token=leak") || strings.Contains(err.Error(), "tenant=private") || strings.Contains(err.Error(), "10.0.0.1") {
			t.Fatalf("error leaked upstream detail: %v", err)
		}
	}
	adapter := New()
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(sentinel)
	})}
	p := provider.Provider{ID: "a2a/ws-0000000000000000/secure-agent", Protocol: "a2a", Name: "secure-agent", Endpoint: "https://secret.internal"}
	_, err := adapter.Discover(context.Background(), p)
	assertSafe(t, err)

	adapter = New()
	adapter.cache[p.ID] = resolvedProvider{baseURL: "https://secret.internal/a2a/v1"}
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New(sentinel)
	})}
	err = adapter.Invoke(context.Background(), p, capability.Capability{}, "safe-error-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
	assertSafe(t, err)

	adapter = New()
	adapter.cache[p.ID] = resolvedProvider{baseURL: "https://secret.internal/a2a/v1"}
	adapter.client = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/a2a+json"}}, Body: io.NopCloser(strings.NewReader(`{"message":{"messageId":"bad","role":"ROLE_AGENT","parts":[{"text":"done"}]}}`))}, nil
	})}
	err = adapter.Invoke(context.Background(), p, capability.Capability{}, "invalid-response-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{})
	assertSafe(t, err)
}

func TestDiscoveryURLUsesOnlyExplicitAgentCardPath(t *testing.T) {
	for _, test := range []struct {
		endpoint string
		want     string
	}{
		{"https://agent.example/a2a/v1?token=unsafe", "https://agent.example/.well-known/agent-card.json"},
		{"https://agent.example/cards/agent-card.json?version=1", "https://agent.example/cards/agent-card.json?version=1"},
	} {
		got, err := discoveryURL(test.endpoint)
		if err != nil || got.String() != test.want {
			t.Fatalf("discoveryURL(%q)=%v, %v; want %q", test.endpoint, got, err, test.want)
		}
	}
}

func TestDiscoverRejectsHTTPSCardToHTTPInterfaceWithoutCredentialLeak(t *testing.T) {
	operationRequests := 0
	operation := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		operationRequests++
		if r.Header.Get("Authorization") != "" {
			t.Error("credential leaked to downgraded interface")
		}
	}))
	defer operation.Close()
	var cardServer *httptest.Server
	cardServer = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("credential leaked during discovery")
		}
		card := validCard(cardServer.URL)
		card["supportedInterfaces"] = []any{map[string]any{"url": operation.URL + "/a2a/v1", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0"}}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}))
	defer cardServer.Close()
	adapter := New()
	adapter.client = cardServer.Client()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: cardServer.URL}
	if _, err := adapter.Discover(context.Background(), p); err == nil {
		t.Fatal("Discover accepted HTTPS AgentCard with HTTP operation interface")
	}
	if operationRequests != 0 {
		t.Fatalf("downgraded operation requests=%d", operationRequests)
	}
}

func TestDiscoverAcceptsHTTPSCardAndHTTPSInterface(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(validCard(server.URL))
	}))
	defer server.Close()
	adapter := New()
	adapter.client = server.Client()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	if _, err := adapter.Discover(context.Background(), p); err != nil {
		t.Fatalf("Discover rejected HTTPS interface: %v", err)
	}
}

func TestDiscoverRejectsCollidingCapabilityIDs(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		card := validCard(server.URL)
		card["skills"] = []any{
			map[string]any{"id": "read.file", "name": "Read dot", "description": "Read a file.", "tags": []string{"read"}},
			map[string]any{"id": "read-file", "name": "Read dash", "description": "Read a file.", "tags": []string{"read"}},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	}))
	defer server.Close()
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	if _, err := adapter.Discover(context.Background(), p); err == nil {
		t.Fatal("Discover accepted colliding capability IDs")
	}
}

func TestValidateCardTracksRequiredCapabilitiesPresence(t *testing.T) {
	for _, test := range []struct {
		name         string
		capabilities json.RawMessage
		wantError    bool
	}{
		{name: "missing", capabilities: nil, wantError: true},
		{name: "null", capabilities: json.RawMessage(`null`), wantError: true},
		{name: "empty object", capabilities: json.RawMessage(`{}`)},
		{name: "populated object", capabilities: json.RawMessage(`{"streaming":false}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, _ := json.Marshal(validCard("https://agent.example"))
			var card agentCard
			if err := json.Unmarshal(encoded, &card); err != nil {
				t.Fatal(err)
			}
			card.Capabilities = test.capabilities
			err := validateCard(card)
			if (err != nil) != test.wantError {
				t.Fatalf("validateCard() error=%v wantError=%v", err, test.wantError)
			}
		})
	}
}

func TestValidateCardRejectsMalformedAndDuplicateExtensions(t *testing.T) {
	for _, capabilities := range []string{
		`{"extensions":[{"uri":""}]}`,
		`{"extensions":[{"uri":"https://extensions.example/a"},{"uri":"https://extensions.example/a"}]}`,
		`{"extensions":[{"uri":"relative-extension"}]}`,
		`{"extensions":[{"uri":"https://extensions.example/a","params":[]}]}`,
	} {
		encoded, _ := json.Marshal(validCard("https://agent.example"))
		var card agentCard
		if err := json.Unmarshal(encoded, &card); err != nil {
			t.Fatal(err)
		}
		card.Capabilities = json.RawMessage(capabilities)
		if err := validateCard(card); err == nil {
			t.Errorf("validateCard accepted capabilities=%s", capabilities)
		}
	}
}

func TestDiscoverRejectsRequiredExtensionBeforeOperation(t *testing.T) {
	operationRequests := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			if r.Header.Get("Authorization") != "" {
				t.Error("discovery leaked bearer token")
			}
			card := validCard(server.URL)
			card["capabilities"] = map[string]any{"extensions": []any{map[string]any{"uri": "https://extensions.example/required", "required": true}}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(card)
		default:
			operationRequests++
			if r.Header.Get("Authorization") != "" {
				t.Error("required extension path leaked bearer token")
			}
		}
	}))
	defer server.Close()
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "unsupported required A2A extension") || capabilities != nil {
		t.Fatalf("Discover capabilities=%#v error=%v", capabilities, err)
	}
	if operationRequests != 0 {
		t.Fatalf("operation requests=%d", operationRequests)
	}
}

func TestOptionalExtensionIsMetadataOnlyAndSendsNoExtensionHeader(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			card := validCard(server.URL)
			card["capabilities"] = map[string]any{"extensions": []any{map[string]any{"uri": "https://extensions.example/optional", "description": "Optional output hints"}}}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(card)
		case "/a2a/v1/message:send":
			if r.Header.Get("A2A-Extensions") != "" {
				t.Errorf("A2A-Extensions=%q", r.Header.Get("A2A-Extensions"))
			}
			w.Header().Set("Content-Type", "application/a2a+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"messageId": "response", "contextId": "context", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "done"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := New()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 1 || !bytes.Contains(capabilities[0].RawMetadata, []byte("https://extensions.example/optional")) {
		t.Fatalf("capabilities=%#v", capabilities)
	}
	if err := adapter.Invoke(context.Background(), p, capabilities[0], "optional-call", json.RawMessage(`{"parts":[{"text":"work"}]}`), &recordingSink{}); err != nil {
		t.Fatal(err)
	}
}

func TestInvokeDirectMessageUsesOfficialRESTShape(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("discovery Authorization=%q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(bearerCard(server.URL))
		case "/a2a/v1/message:send":
			if got := r.Header.Get("Authorization"); got != "Bearer provider-secret" {
				t.Errorf("operation Authorization=%q", got)
			}
			if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/a2a+json" || r.Header.Get("Accept") != "application/a2a+json" || r.Header.Get("A2A-Version") != "1.0" {
				t.Errorf("request method/headers=%s %#v", r.Method, r.Header)
			}
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
			}
			message, _ := request["message"].(map[string]any)
			configuration, _ := request["configuration"].(map[string]any)
			if request["tenant"] != "tenant-a" || message["role"] != "ROLE_USER" || message["messageId"] == "" || configuration["returnImmediately"] != true {
				t.Errorf("request=%#v", request)
			}
			if _, exists := request["skillId"]; exists {
				t.Errorf("nonstandard skillId sent: %#v", request)
			}
			w.Header().Set("Content-Type", "application/a2a+json")
			_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"messageId": "response-1", "contextId": "context-1", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "done"}}}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	adapter := NewWithResolver(staticResolver{"research-agent.token": "provider-secret"})
	p := bearerProvider("research-agent", server.URL)
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	if capabilities[0].Security.UpstreamAuth != "secret" {
		t.Fatalf("upstream auth=%q", capabilities[0].Security.UpstreamAuth)
	}
	sink := &recordingSink{}
	input := json.RawMessage(`{"parts":[{"text":"summarize"}],"configuration":{"acceptedOutputModes":["text/plain"]},"metadata":{"request":"safe"}}`)
	if err := adapter.Invoke(context.Background(), p, capabilities[0], "call-direct", input, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.events) != 1 || sink.events[0].Type != "result" || len(sink.order) != 2 || sink.order[0] != "started" || sink.order[1] != "event:result" {
		t.Fatalf("sink order/events=%v %#v", sink.order, sink.events)
	}
	var message map[string]any
	if err := json.Unmarshal(sink.events[0].Data, &message); err != nil || message["messageId"] != "response-1" {
		t.Fatalf("result=%s err=%v", sink.events[0].Data, err)
	}
}

func TestInvokeAcceptsOfficialPartOptionalFields(t *testing.T) {
	input, err := parseInvokeInput(json.RawMessage(`{"parts":[{"text":"hello","metadata":{"source":"user"},"filename":"prompt.txt","mediaType":"text/plain"}]}`))
	if err != nil || len(input.Parts) != 1 {
		t.Fatalf("parseInvokeInput()=%#v, %v", input, err)
	}
}

func TestInvokeInputSchemaMatchesParserBoundaries(t *testing.T) {
	schema := invokeInputSchema()
	valid := []string{
		`{"parts":[{"text":"hello"}]}`,
		`{"parts":[{"raw":"AA==","filename":"payload.bin","mediaType":"application/octet-stream"}]}`,
		`{"parts":[{"url":"https://files.example/report.pdf"}]}`,
		`{"parts":[{"data":null}],"configuration":{"acceptedOutputModes":["application/json"],"historyLength":2},"metadata":{}}`,
	}
	for _, input := range valid {
		data := json.RawMessage(input)
		if err := jsoncontract.Validate(schema, data); err != nil {
			t.Errorf("schema rejected valid input %s: %v", input, err)
		}
		if _, err := parseInvokeInput(data); err != nil {
			t.Errorf("parser rejected valid input %s: %v", input, err)
		}
	}

	invalid := []string{
		`{"parts":[{}]}`,
		`{"parts":[{"text":"one","data":2}]}`,
		`{"parts":[{"raw":"not base64"}]}`,
		`{"parts":[{"url":"relative/path"}]}`,
		`{"parts":[{"url":"https:///missing-host"}]}`,
		`{"parts":[{"url":"http:relative"}]}`,
		`{"parts":[{"url":"https://files.example/%zz"}]}`,
		`{"parts":[{"text":"ok","filename":""}]}`,
		`{"parts":[{"text":"ok","mediaType":"not a media type"}]}`,
		`{"parts":[{"text":"ok","unknown":true}]}`,
		`{"parts":[{"text":"ok"}],"configuration":{"returnImmediately":false}}`,
	}
	for _, input := range invalid {
		data := json.RawMessage(input)
		if err := jsoncontract.Validate(schema, data); !errors.Is(err, jsoncontract.ErrInvalidValue) {
			t.Errorf("schema accepted invalid input %s: %v", input, err)
		}
		if _, err := parseInvokeInput(data); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("parser accepted invalid input %s: %v", input, err)
		}
	}
}

func TestInvokeRejectsMalformedPartOptionalFields(t *testing.T) {
	for _, input := range []string{
		`{"parts":[{"text":"hello","filename":7}]}`,
		`{"parts":[{"text":"hello","metadata":[]}]}`,
		`{"parts":[{"text":"hello","data":{"also":"content"}}]}`,
	} {
		if _, err := parseInvokeInput(json.RawMessage(input)); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("parseInvokeInput(%s) error=%v", input, err)
		}
	}
}

func TestPartOneofAcceptsA2A10JSONValuesAndValidFileContent(t *testing.T) {
	valid := []string{
		`{"text":""}`,
		`{"raw":"AA==","filename":"payload.bin","mediaType":"application/octet-stream"}`,
		`{"url":"https://files.example/report.pdf","filename":"report.pdf","mediaType":"application/pdf"}`,
		`{"data":{}}`, `{"data":[]}`, `{"data":"value"}`, `{"data":42}`, `{"data":true}`, `{"data":null}`,
	}
	for _, part := range valid {
		input := json.RawMessage(`{"parts":[` + part + `]}`)
		if _, err := parseInvokeInput(input); err != nil {
			t.Errorf("request part %s rejected: %v", part, err)
		}
		message := json.RawMessage(`{"messageId":"response","contextId":"context","role":"ROLE_AGENT","parts":[` + part + `]}`)
		if err := validateResponseMessage(message); err != nil {
			t.Errorf("response part %s rejected: %v", part, err)
		}
		task := taskResponse{ID: "task", Status: json.RawMessage(`{"state":"TASK_STATE_WORKING"}`), Artifacts: []json.RawMessage{json.RawMessage(`{"artifactId":"artifact","parts":[` + part + `]}`)}}
		if _, err := emitTaskUpdate(task, taskStatus{State: "TASK_STATE_WORKING"}, new(string), map[string][32]byte{}, &recordingSink{}); err != nil {
			t.Errorf("artifact part %s rejected: %v", part, err)
		}
	}
}

func TestPartOneofRejectsInvalidA2A10ContentAcrossBoundaries(t *testing.T) {
	invalid := []string{
		`{}`,
		`{"text":"one","data":2}`,
		`{"raw":"not base64"}`,
		`{"url":""}`,
		`{"url":"relative/path"}`,
		`{"text":"ok","filename":""}`,
		`{"text":"ok","mediaType":"not a media type"}`,
		`{"file":{"bytes":"AA=="}}`,
	}
	for _, part := range invalid {
		input := json.RawMessage(`{"parts":[` + part + `]}`)
		if _, err := parseInvokeInput(input); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("request part %s error=%v", part, err)
		}
		message := json.RawMessage(`{"messageId":"response","contextId":"context","role":"ROLE_AGENT","parts":[` + part + `]}`)
		if err := validateResponseMessage(message); err == nil {
			t.Errorf("response part %s accepted", part)
		}
		task := taskResponse{ID: "task", Status: json.RawMessage(`{"state":"TASK_STATE_WORKING"}`), Artifacts: []json.RawMessage{json.RawMessage(`{"artifactId":"artifact","parts":[` + part + `]}`)}}
		if _, err := emitTaskUpdate(task, taskStatus{State: "TASK_STATE_WORKING"}, new(string), map[string][32]byte{}, &recordingSink{}); err == nil {
			t.Errorf("artifact part %s accepted", part)
		}
	}
}

func TestValidateResponseMessageRequiresServerContextID(t *testing.T) {
	missing := json.RawMessage(`{"messageId":"response","role":"ROLE_AGENT","parts":[{"text":"done"}]}`)
	if err := validateResponseMessage(missing); err == nil {
		t.Fatal("server Message without contextId was accepted")
	}
	valid := json.RawMessage(`{"messageId":"response","contextId":"context","role":"ROLE_AGENT","parts":[{"text":"done"}]}`)
	if err := validateResponseMessage(valid); err != nil {
		t.Fatalf("valid server Message rejected: %v", err)
	}
}

func TestDirectResponseMessageRejectsTaskID(t *testing.T) {
	message := json.RawMessage(`{"messageId":"response","contextId":"context","taskId":"unexpected-task","role":"ROLE_AGENT","parts":[{"text":"done"}]}`)
	if err := validateResponseMessage(message); err == nil {
		t.Fatal("direct server Message with taskId was accepted")
	}
}

func TestInvokeInputRejectsCallerContinuationFields(t *testing.T) {
	for _, field := range []string{`"taskId":"task",`, `"contextId":"context",`} {
		input := json.RawMessage(`{` + field + `"parts":[{"text":"continue"}]}`)
		if _, err := parseInvokeInput(input); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("parseInvokeInput(%s) error=%v", input, err)
		}
	}
}

func TestParseTaskValidatesStatusAndHistoryMessages(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
	}{
		{
			name: "status server message missing context",
			raw:  `{"id":"task","contextId":"context","status":{"state":"TASK_STATE_WORKING","message":{"messageId":"status","taskId":"task","role":"ROLE_AGENT","parts":[{"text":"working"}]}}}`,
		},
		{
			name: "history server message missing context",
			raw:  `{"id":"task","contextId":"context","status":{"state":"TASK_STATE_WORKING"},"history":[{"messageId":"history","taskId":"task","role":"ROLE_AGENT","parts":[{"text":"working"}]}]}`,
		},
		{
			name: "history task mismatch",
			raw:  `{"id":"task","contextId":"context","status":{"state":"TASK_STATE_WORKING"},"history":[{"messageId":"history","contextId":"context","taskId":"other","role":"ROLE_USER","parts":[{"text":"work"}]}]}`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, _, err := parseTask(json.RawMessage(test.raw), "task"); err == nil {
				t.Fatal("parseTask accepted invalid nested Message")
			}
		})
	}
	valid := json.RawMessage(`{"id":"task","contextId":"context","status":{"state":"TASK_STATE_WORKING","message":{"messageId":"status","contextId":"context","taskId":"task","role":"ROLE_AGENT","parts":[{"text":"working"}]}},"history":[{"messageId":"request","contextId":"context","taskId":"task","role":"ROLE_USER","parts":[{"text":"work"}]}]}`)
	if _, _, err := parseTask(valid, "task"); err != nil {
		t.Fatalf("parseTask rejected valid nested Messages: %v", err)
	}
}

func TestHealthResolvesOnMissAndCloseInvalidatesCache(t *testing.T) {
	discoveries := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		discoveries++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(validCard(server.URL))
	}))
	defer server.Close()
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	adapter := New()
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("Health()=%#v", health)
	}
	if health := adapter.Health(context.Background(), p); !health.Healthy || discoveries != 1 {
		t.Fatalf("cached Health()=%#v discoveries=%d", health, discoveries)
	}
	if err := adapter.Close(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	if health := adapter.Health(context.Background(), p); !health.Healthy || discoveries != 2 {
		t.Fatalf("Health after Close()=%#v discoveries=%d", health, discoveries)
	}
}

func TestInvokeRejectsCallerReturnImmediatelyOverride(t *testing.T) {
	adapter := New()
	input := json.RawMessage(`{"parts":[{"text":"hello"}],"configuration":{"returnImmediately":false}}`)
	err := adapter.Invoke(context.Background(), provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: "http://127.0.0.1"}, capability.Capability{}, "call", input, &recordingSink{})
	if err == nil || !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Invoke error=%v", err)
	}
}

func TestInvokePollsTaskWithOrderedDeduplicatedEvents(t *testing.T) {
	polls := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/a2a+json")
		switch r.URL.EscapedPath() {
		case "/.well-known/agent-card.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(validCard(server.URL))
		case "/a2a/v1/message:send":
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{
				"id": "remote/task 1", "contextId": "context-1", "status": map[string]any{"state": "TASK_STATE_SUBMITTED"},
				"artifacts": []any{map[string]any{"artifactId": "artifact-1", "name": "First Report", "parts": []any{map[string]any{"text": "first"}}}},
			}})
		case "/a2a/v1/tasks/remote%2Ftask%201":
			if got := r.URL.Query().Get("tenant"); got != "tenant-a" {
				t.Errorf("poll tenant=%q", got)
			}
			polls++
			state := "TASK_STATE_WORKING"
			artifacts := []any{map[string]any{"artifactId": "artifact-1", "name": "First Report", "parts": []any{map[string]any{"text": "first"}}}}
			if polls >= 2 {
				state = "TASK_STATE_COMPLETED"
				artifacts = append(artifacts, map[string]any{"artifactId": "artifact-2", "name": "Final Report", "parts": []any{map[string]any{"data": map[string]any{"ok": true}}}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "remote/task 1", "contextId": "context-1", "status": map[string]any{"state": state}, "artifacts": artifacts})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	adapter := New()
	adapter.pollInterval = time.Millisecond
	p := provider.Provider{ID: "a2a/ws-0000000000000000/research-agent", Protocol: "a2a", Name: "research-agent", Endpoint: server.URL}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	if err := adapter.Invoke(context.Background(), p, capabilities[0], "call-task", json.RawMessage(`{"parts":[{"text":"work"}]}`), sink); err != nil {
		t.Fatal(err)
	}
	if polls != 2 || len(sink.artifacts) != 2 {
		t.Fatalf("polls=%d artifacts=%#v order=%v", polls, sink.artifacts, sink.order)
	}
	joined := strings.Join(sink.order, ",")
	if joined != "started,event:status,artifact:first-report,event:status,event:status,artifact:final-report,event:result" {
		t.Fatalf("order=%s", joined)
	}
	if len(sink.events) != 4 || sink.events[len(sink.events)-1].Type != "result" || !strings.Contains(string(sink.events[len(sink.events)-1].Data), `"TASK_STATE_COMPLETED"`) {
		t.Fatalf("events=%#v", sink.events)
	}
}

func TestArtifactSnapshotsDedupeIdenticalContentAndEmitRevisions(t *testing.T) {
	sink := &recordingSink{}
	previousStatus := ""
	seen := map[string][32]byte{}
	updates := []json.RawMessage{
		json.RawMessage(`{"artifactId":"report","name":"Draft","metadata":{"stage":1},"parts":[{"text":"v1"}]}`),
		json.RawMessage(`{"parts":[{"text":"v1"}],"metadata":{"stage":1},"name":"Draft","artifactId":"report"}`),
		json.RawMessage(`{"artifactId":"report","name":"Draft","metadata":{"stage":2},"parts":[{"text":"v2"}]}`),
		json.RawMessage(`{"artifactId":"report","name":"Final","metadata":{"stage":3},"parts":[{"text":"final"}]}`),
	}
	for i, artifact := range updates {
		task := taskResponse{ID: "task", Status: json.RawMessage(`{"state":"TASK_STATE_WORKING"}`), Artifacts: []json.RawMessage{artifact}}
		if _, err := emitTaskUpdate(task, taskStatus{State: "TASK_STATE_WORKING"}, &previousStatus, seen, sink); err != nil {
			t.Fatalf("update %d: %v", i, err)
		}
	}
	if len(sink.artifacts) != 3 || !strings.Contains(sink.artifacts[2], `"text":"final"`) {
		t.Fatalf("artifacts=%#v", sink.artifacts)
	}
}
