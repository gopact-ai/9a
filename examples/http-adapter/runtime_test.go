package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(value)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func requestLine(id, method string, params any) string {
	encoded, _ := json.Marshal(map[string]any{"version": adapterProtocolVersion, "id": id, "method": method, "params": params})
	return string(encoded) + "\n"
}

func decodeProtocolLines(t *testing.T, output string) []protocolResponse {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(output), "\n")
	responses := make([]protocolResponse, 0, len(lines))
	for _, line := range lines {
		var response protocolResponse
		if err := json.Unmarshal([]byte(line), &response); err != nil {
			t.Fatalf("invalid protocol line %q: %v", line, err)
		}
		if response.Version != adapterProtocolVersion || response.ID == "" {
			t.Fatalf("response=%#v", response)
		}
		responses = append(responses, response)
	}
	return responses
}

func TestRuntimeDiscoverHealthInvokeCancelAndProtocolErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/healthz" {
			_, _ = io.WriteString(w, `{"healthy":true}`)
			return
		}
		_, _ = io.WriteString(w, `{"value":42}`)
	}))
	defer server.Close()
	m := testManifest(t, testOperation("read", "POST", "/read", "none"))
	m.HealthPath, m.HealthAuth = "/healthz", "none"
	provider := map[string]any{"name": "public-api", "endpoint": server.URL}
	input := strings.Join([]string{
		requestLine("discover-1", "discover", map[string]any{"provider": provider}),
		requestLine("health-1", "health", map[string]any{"provider": provider}),
		requestLine("invoke-1", "invoke", map[string]any{"provider": provider, "capability": map[string]any{"upstream_name": "read"}, "input": map[string]any{"id": "123"}}),
		requestLine("cancel-1", "cancel", map[string]any{"invocation_id": "invoke-1"}),
		requestLine("method-1", "unknown", map[string]any{}),
		`{"version":"wrong","id":"version-1","method":"health","params":{}}` + "\n",
	}, "")
	var output, diagnostics synchronizedBuffer
	runtime := newRuntime(m, strings.NewReader(input), &output, &diagnostics)
	if err := runtime.serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	responses := decodeProtocolLines(t, output.String())
	if len(responses) != 6 {
		t.Fatalf("responses=%d output=%s", len(responses), output.String())
	}
	byID := map[string]protocolResponse{}
	for _, response := range responses {
		byID[response.ID] = response
	}
	var discover struct {
		Capabilities []capabilityDTO `json:"capabilities"`
	}
	if err := json.Unmarshal(byID["discover-1"].Result, &discover); err != nil || len(discover.Capabilities) != 1 {
		t.Fatalf("discover=%s err=%v", byID["discover-1"].Result, err)
	}
	capability := discover.Capabilities[0]
	if capability.UpstreamName != "read" || capability.Kind != "api.operation" || capability.Input.JSONSchema["type"] != "object" || capability.Lifecycle != (lifecycleDTO{Sync: true}) || capability.Security.UpstreamAuth != "none" {
		t.Fatalf("capability=%#v", capability)
	}
	if !bytes.Contains(byID["health-1"].Result, []byte(`"healthy":true`)) || !bytes.Contains(byID["invoke-1"].Result, []byte(`"output":{"value":42}`)) {
		t.Fatalf("health=%s invoke=%s", byID["health-1"].Result, byID["invoke-1"].Result)
	}
	if byID["cancel-1"].Error == nil || byID["cancel-1"].Error.Code != "not_cancelable" || byID["method-1"].Error == nil || byID["method-1"].Error.Code != "unsupported_method" || byID["version-1"].Error == nil || byID["version-1"].Error.Code != "unsupported_version" {
		t.Fatalf("errors cancel=%#v method=%#v version=%#v", byID["cancel-1"], byID["method-1"], byID["version-1"])
	}
	if diagnostics.String() != "" {
		t.Fatalf("diagnostics=%q", diagnostics.String())
	}
}

func TestRuntimeConcurrentResponsesAreWholeAndKeepRequestIDs(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()
	provider := map[string]any{"name": "public-api", "endpoint": server.URL}
	var input strings.Builder
	const count = 200
	for i := 0; i < count; i++ {
		input.WriteString(requestLine(fmt.Sprintf("request-%d", i), "health", map[string]any{"provider": provider}))
	}
	var output, diagnostics synchronizedBuffer
	runtime := newRuntime(testManifest(t, testOperation("read", "POST", "/read", "none")), strings.NewReader(input.String()), &output, &diagnostics)
	if err := runtime.serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	responses := decodeProtocolLines(t, output.String())
	if len(responses) != count {
		t.Fatalf("response count=%d", len(responses))
	}
	seen := map[string]bool{}
	for _, response := range responses {
		if seen[response.ID] || response.Error != nil || response.Result == nil {
			t.Fatalf("response=%#v duplicate=%v", response, seen[response.ID])
		}
		seen[response.ID] = true
	}
}

func TestRuntimeBoundsActiveInvokesAndReleasesSlots(t *testing.T) {
	const limit = 32
	release := make(chan struct{})
	started := make(chan struct{}, limit)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	provider := map[string]any{"name": "public-api", "endpoint": server.URL}
	var input strings.Builder
	for i := 0; i < limit+1; i++ {
		input.WriteString(requestLine(fmt.Sprintf("invoke-%d", i), "invoke", map[string]any{"provider": provider, "capability": map[string]any{"upstream_name": "read"}, "input": map[string]any{}}))
	}
	var output, diagnostics synchronizedBuffer
	runtime := newRuntime(testManifest(t, testOperation("read", "POST", "/read", "none")), strings.NewReader(input.String()), &output, &diagnostics)
	done := make(chan error, 1)
	go func() { done <- runtime.serve(context.Background()) }()
	for i := 0; i < limit; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			close(release)
			t.Fatalf("only %d invokes reached HTTP", i)
		}
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(output.String(), "resource_exhausted") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(output.String(), "resource_exhausted") {
		close(release)
		t.Fatal("33rd invoke did not fail immediately")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	responses := decodeProtocolLines(t, output.String())
	if len(responses) != limit+1 {
		t.Fatalf("responses=%d", len(responses))
	}
	var exhausted int
	for _, response := range responses {
		if response.Error != nil && response.Error.Code == "resource_exhausted" {
			exhausted++
		}
	}
	if exhausted != 1 {
		t.Fatalf("resource exhausted responses=%d", exhausted)
	}
}

func TestRuntimeRejectsOversizedAndInvalidUTF8InputWithoutStdoutNoise(t *testing.T) {
	for _, input := range [][]byte{
		append(bytes.Repeat([]byte(" "), maxProtocolLineBytes), '\n'),
		{0xff, '\n'},
	} {
		var output, diagnostics synchronizedBuffer
		runtime := newRuntime(testManifest(t, testOperation("read", "POST", "/read", "none")), bytes.NewReader(input), &output, &diagnostics)
		if err := runtime.serve(context.Background()); err == nil {
			t.Fatal("serve accepted invalid protocol input")
		}
		if output.String() != "" || diagnostics.String() == "" {
			t.Fatalf("stdout=%q stderr=%q", output.String(), diagnostics.String())
		}
	}
}

func TestRuntimeOversizedInvokeEnvelopeFallsBackAndSessionRemainsUsable(t *testing.T) {
	largeJSON := `"` + strings.Repeat("x", maxHTTPBodyBytes-2) + `"`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, largeJSON)
	}))
	defer server.Close()
	provider := map[string]any{"name": "public-api", "endpoint": server.URL}
	oversizedID := strings.Repeat("i", maxManifestStringBytes)
	var input strings.Builder
	input.WriteString(requestLine(oversizedID, "invoke", map[string]any{"provider": provider, "capability": map[string]any{"upstream_name": "large"}, "input": map[string]any{}}))
	const healthCount = 40
	for i := 0; i < healthCount; i++ {
		input.WriteString(requestLine(fmt.Sprintf("health-after-large-%d", i), "health", map[string]any{"provider": provider}))
	}
	var output, diagnostics synchronizedBuffer
	runtime := newRuntime(testManifest(t, testOperation("large", "POST", "/large", "none")), strings.NewReader(input.String()), &output, &diagnostics)
	if err := runtime.serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != healthCount+1 {
		t.Fatalf("line count=%d", len(lines))
	}
	responses := decodeProtocolLines(t, output.String())
	counts := map[string]int{}
	var oversized protocolResponse
	for i, response := range responses {
		counts[response.ID]++
		if len(lines[i])+1 > maxProtocolLineBytes {
			t.Fatalf("response line %d bytes=%d", i, len(lines[i])+1)
		}
		if response.ID == oversizedID {
			oversized = response
		} else if response.Error != nil || !bytes.Contains(response.Result, []byte(`"healthy":true`)) {
			t.Fatalf("follow-up response=%#v", response)
		}
	}
	if counts[oversizedID] != 1 || oversized.Error == nil || oversized.Error.Code != "response_too_large" || oversized.Error.Message != "adapter response exceeds limit" || oversized.Result != nil {
		t.Fatalf("oversized count=%d response=%#v", counts[oversizedID], oversized)
	}
}

func TestRuntimeOversizedDiscoverEnvelopeFallsBackAndSessionRemainsUsable(t *testing.T) {
	operations := make([]operation, 0, maxManifestOperations)
	for i := 0; i < maxManifestOperations; i++ {
		item := testOperation(fmt.Sprintf("operation-%d", i), "GET", fmt.Sprintf("/operation-%d", i), "none")
		item.Name = "Operation"
		item.Description = strings.Repeat("d", 500)
		operations = append(operations, item)
	}
	configuration := testManifest(t, operations...)
	source, err := json.Marshal(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if len(source) > maxManifestBytes {
		t.Fatalf("test manifest exceeds source limit: %d", len(source))
	}
	provider := map[string]any{"name": "public-api", "endpoint": "https://api.example"}
	discoverID := strings.Repeat("d", maxManifestStringBytes)
	input := requestLine(discoverID, "discover", map[string]any{"provider": provider}) + requestLine("health-after-discover", "health", map[string]any{"provider": provider})
	var output, diagnostics synchronizedBuffer
	runtime := newRuntime(configuration, strings.NewReader(input), &output, &diagnostics)
	if err := runtime.serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	responses := decodeProtocolLines(t, output.String())
	if len(responses) != 2 {
		t.Fatalf("responses=%d", len(responses))
	}
	byID := map[string]protocolResponse{}
	for _, response := range responses {
		byID[response.ID] = response
	}
	oversized := byID[discoverID]
	if oversized.Error == nil || oversized.Error.Code != "response_too_large" || oversized.Error.Message != "adapter response exceeds limit" || oversized.Result != nil {
		t.Fatalf("discover response=%#v", oversized)
	}
	if followup := byID["health-after-discover"]; followup.Error != nil || !bytes.Contains(followup.Result, []byte(`"healthy":true`)) {
		t.Fatalf("follow-up=%#v", followup)
	}
	for i, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if len(line)+1 > maxProtocolLineBytes {
			t.Fatalf("response line %d bytes=%d", i, len(line)+1)
		}
	}
}

func TestRuntimeRejectsUnterminatedRequestsWithoutDispatch(t *testing.T) {
	upstreamCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	provider := map[string]any{"name": "public-api", "endpoint": server.URL}
	for _, test := range []struct {
		name   string
		method string
		params any
	}{
		{name: "invoke", method: "invoke", params: map[string]any{"provider": provider, "capability": map[string]any{"upstream_name": "read"}, "input": map[string]any{}}},
		{name: "discover", method: "discover", params: map[string]any{"provider": provider}},
	} {
		t.Run(test.name, func(t *testing.T) {
			input := strings.TrimSuffix(requestLine("unterminated-"+test.name, test.method, test.params), "\n")
			var output, diagnostics synchronizedBuffer
			runtime := newRuntime(testManifest(t, testOperation("read", "POST", "/read", "none")), strings.NewReader(input), &output, &diagnostics)
			err := runtime.serve(context.Background())
			if err == nil || !strings.Contains(err.Error(), "newline") {
				t.Fatalf("serve error=%v", err)
			}
			if output.String() != "" {
				t.Fatalf("stdout=%q", output.String())
			}
		})
	}
	if upstreamCalls != 0 {
		t.Fatalf("unterminated invoke reached upstream %d times", upstreamCalls)
	}
}

func TestRuntimeAcceptsTerminatedRequestsAndCleanEOF(t *testing.T) {
	provider := map[string]any{"name": "public-api", "endpoint": "https://api.example"}
	for _, terminator := range []string{"\n", "\r\n"} {
		input := strings.TrimSuffix(requestLine("discover-control", "discover", map[string]any{"provider": provider}), "\n") + terminator
		var output, diagnostics synchronizedBuffer
		runtime := newRuntime(testManifest(t, testOperation("read", "POST", "/read", "none")), strings.NewReader(input), &output, &diagnostics)
		if err := runtime.serve(context.Background()); err != nil {
			t.Fatalf("terminator %q: %v", terminator, err)
		}
		responses := decodeProtocolLines(t, output.String())
		if len(responses) != 1 || responses[0].ID != "discover-control" || responses[0].Error != nil {
			t.Fatalf("terminator %q responses=%#v", terminator, responses)
		}
	}
	var output, diagnostics synchronizedBuffer
	if err := newRuntime(testManifest(t, testOperation("read", "POST", "/read", "none")), strings.NewReader(""), &output, &diagnostics).serve(context.Background()); err != nil || output.String() != "" {
		t.Fatalf("clean EOF error=%v stdout=%q", err, output.String())
	}
}

func TestRuntimeEOFWaitsForNewlineTerminatedInFlightInvoke(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	provider := map[string]any{"name": "public-api", "endpoint": server.URL}
	input := requestLine("terminated-invoke", "invoke", map[string]any{"provider": provider, "capability": map[string]any{"upstream_name": "read"}, "input": map[string]any{}})
	var output, diagnostics synchronizedBuffer
	runtime := newRuntime(testManifest(t, testOperation("read", "POST", "/read", "none")), strings.NewReader(input), &output, &diagnostics)
	done := make(chan error, 1)
	go func() { done <- runtime.serve(context.Background()) }()
	<-started
	select {
	case err := <-done:
		t.Fatalf("serve returned before in-flight invoke: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	responses := decodeProtocolLines(t, output.String())
	if len(responses) != 1 || responses[0].ID != "terminated-invoke" || responses[0].Error != nil {
		t.Fatalf("responses=%#v", responses)
	}
}

func TestRuntimeAcceptsEightMiBLineIncludingNewline(t *testing.T) {
	prefix := `{"version":"9a.adapter/v1","id":"boundary","method":"health","params":{"provider":{"name":"public-api","endpoint":"https://api.example"}},"padding":"`
	suffix := `"}`
	paddingBytes := maxProtocolLineBytes - len(prefix) - len(suffix) - 1
	if paddingBytes < 0 {
		t.Fatal("boundary fixture is larger than protocol limit")
	}
	input := prefix + strings.Repeat("x", paddingBytes) + suffix + "\n"
	if len(input) != maxProtocolLineBytes {
		t.Fatalf("fixture bytes=%d", len(input))
	}
	var output, diagnostics synchronizedBuffer
	runtime := newRuntime(testManifest(t, testOperation("read", "POST", "/read", "none")), strings.NewReader(input), &output, &diagnostics)
	if err := runtime.serve(context.Background()); err != nil {
		t.Fatal(err)
	}
	responses := decodeProtocolLines(t, output.String())
	if len(responses) != 1 || responses[0].ID != "boundary" || responses[0].Error != nil {
		t.Fatalf("responses=%#v", responses)
	}
}
