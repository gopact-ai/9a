package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/processgroup"
	"github.com/gopact-ai/9a/internal/provider"
)

const mcpHelperEnv = "NINEA_MCP_HELPER"
const mcpHelperToolResultEnv = "NINEA_MCP_HELPER_TOOL_RESULT"
const mcpHelperCaptureParamsEnv = "NINEA_MCP_HELPER_CAPTURE_PARAMS"
const mcpHelperLargeSchemaEnv = "NINEA_MCP_HELPER_LARGE_SCHEMA"

func TestMCPHelperProcess(t *testing.T) {
	mode := os.Getenv(mcpHelperEnv)
	if mode == "" {
		return
	}
	if mode == "holder" {
		for {
			time.Sleep(time.Second)
		}
	}
	if os.Getenv("NINEA_MCP_HELPER_DESCENDANT") == "1" {
		child := exec.Command(os.Args[0], "-test.run=^TestMCPHelperProcess$")
		child.Env = append(os.Environ(), mcpHelperEnv+"=holder")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if path := os.Getenv("NINEA_MCP_HELPER_DESCENDANT_PID"); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(child.Process.Pid)), 0o600)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	writeResponse := func(value any) {
		data, err := json.Marshal(value)
		if err != nil {
			os.Exit(5)
		}
		_, _ = os.Stdout.Write(data)
		if os.Getenv("NINEA_MCP_HELPER_UNTERMINATED") == "1" {
			os.Exit(0)
		}
		if os.Getenv("NINEA_MCP_HELPER_CRLF") == "1" {
			_, _ = os.Stdout.WriteString("\r\n")
		} else {
			_, _ = os.Stdout.WriteString("\n")
		}
	}
	for scanner.Scan() {
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      *int            `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			os.Exit(4)
		}
		if request.ID == nil {
			continue
		}
		switch request.Method {
		case "initialize":
			writeResponse(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": map[string]any{"protocolVersion": "2025-11-25"}})
		case "tools/list":
			tool := map[string]any{"name": "echo", "description": "Echo input.", "inputSchema": map[string]any{"type": "object"}}
			if os.Getenv(mcpHelperLargeSchemaEnv) == "1" {
				tool["inputSchema"] = map[string]any{"const": json.Number("9007199254740993")}
				tool["outputSchema"] = map[string]any{"maximum": json.Number("9007199254740993")}
			}
			if os.Getenv("NINEA_MCP_HELPER_READ_ONLY") == "1" {
				tool["annotations"] = map[string]any{"readOnlyHint": true}
			}
			tools := []any{tool}
			if os.Getenv("NINEA_MCP_HELPER_EMPTY_TOOLS") == "1" {
				tools = []any{}
			}
			writeResponse(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": map[string]any{"tools": tools}})
		case "tools/call":
			if path := os.Getenv(mcpHelperCaptureParamsEnv); path != "" {
				_ = os.WriteFile(path, request.Params, 0o600)
			}
			if ready := os.Getenv("NINEA_MCP_HELPER_READY"); ready != "" {
				_ = os.WriteFile(ready, []byte("ready"), 0o600)
			}
			if os.Getenv("NINEA_MCP_HELPER_EXIT") == "1" {
				os.Exit(0)
			}
			if os.Getenv("NINEA_MCP_HELPER_HANG") == "1" {
				for {
					time.Sleep(time.Second)
				}
			}
			if os.Getenv("NINEA_MCP_HELPER_TOOL_ERROR") == "1" {
				writeResponse(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": "sensitive upstream failure"}}}})
				continue
			}
			if raw := os.Getenv(mcpHelperToolResultEnv); raw != "" {
				var result any
				if json.Unmarshal([]byte(raw), &result) != nil {
					os.Exit(6)
				}
				writeResponse(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": result})
				continue
			}
			writeResponse(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}}})
		}
	}
	os.Exit(0)
}

func mcpHelperExecutable(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "mcp-helper")
	var declarations strings.Builder
	for _, key := range []string{
		mcpHelperEnv, mcpHelperToolResultEnv, mcpHelperCaptureParamsEnv, mcpHelperLargeSchemaEnv, "GORACE",
		"NINEA_MCP_HELPER_CRLF", "NINEA_MCP_HELPER_DESCENDANT", "NINEA_MCP_HELPER_DESCENDANT_PID",
		"NINEA_MCP_HELPER_EMPTY_TOOLS", "NINEA_MCP_HELPER_EXIT", "NINEA_MCP_HELPER_HANG", "NINEA_MCP_HELPER_READY",
		"NINEA_MCP_HELPER_READ_ONLY", "NINEA_MCP_HELPER_TOOL_ERROR", "NINEA_MCP_HELPER_UNTERMINATED",
	} {
		if value, ok := os.LookupEnv(key); ok {
			fmt.Fprintf(&declarations, "export %s=%q\n", key, value)
		}
	}
	script := fmt.Sprintf("#!/bin/sh\n%sexec %q -test.run=^TestMCPHelperProcess$\n", declarations.String(), binary)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForMCPFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForMCPLineCount(t *testing.T, path string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && bytes.Count(data, []byte{'\n'}) >= want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("timed out waiting for %d lines in %s; got %d", want, path, bytes.Count(data, []byte{'\n'}))
}

func mcpShellExecutable(t *testing.T, spawnPath, readyPath string, hang bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "mcp-shell-helper")
	hangCommand := ""
	if hang {
		hangCommand = fmt.Sprintf("printf 'ready\\n' >> %q\nwhile :; do sleep 1; done", readyPath)
	}
	script := fmt.Sprintf(`#!/bin/sh
printf 'spawn\n' >> %q
IFS= read -r request || exit 2
printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}'
IFS= read -r notification || exit 3
IFS= read -r request || exit 4
%s
printf '%%s\n' '{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ok"}]}}'
`, spawnPath, hangCommand)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func mcpActiveSessionCount(adapter *Adapter) int {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	return adapter.activeSessions
}

func readMCPPID(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	return pid
}

func waitForMCPProcessGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if mcpTestProcessTerminated(pid) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d is still alive", pid)
}

func mcpTestProcessTerminated(pid int) bool {
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

type mcpRecordingSink struct{ events []provider.Event }

func (*mcpRecordingSink) Started() error { return nil }
func (s *mcpRecordingSink) Event(event provider.Event) error {
	s.events = append(s.events, event)
	return nil
}
func (*mcpRecordingSink) Artifact(string, string, []byte) error { return nil }

func mcpTestProvider(executable string) provider.Provider {
	return provider.Provider{ID: "mcp/test", Protocol: "mcp", Name: "test", Endpoint: "stdio:" + executable}
}

func mcpTestCapability() capability.Capability {
	return capability.Capability{Source: capability.Source{UpstreamName: "echo"}}
}

func TestDiscoveryBudgetRejectsAggregateAbuse(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		steps []struct {
			bytes, tools int
			cursor       string
		}
	}{{"bytes", []struct {
		bytes, tools int
		cursor       string
	}{{maxDiscoveryBytes + 1, 1, ""}}}, {"tools", []struct {
		bytes, tools int
		cursor       string
	}{{1, maxDiscoveryTools + 1, ""}}}, {"reused cursor", []struct {
		bytes, tools int
		cursor       string
	}{{1, 1, "a"}, {1, 1, "b"}, {1, 1, "a"}}}}
	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			var b discoveryBudget
			var err error
			for _, s := range tt.steps {
				err = b.add(s.bytes, s.tools, s.cursor)
			}
			if err == nil {
				t.Fatal("abusive discovery accepted")
			}
		})
	}
}

func TestInvokeContextCancellationTerminatesMCPProcessTree(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	pidPath := filepath.Join(t.TempDir(), "descendant-pid")
	t.Setenv(mcpHelperEnv, "server")
	t.Setenv("NINEA_MCP_HELPER_DESCENDANT", "1")
	t.Setenv("NINEA_MCP_HELPER_DESCENDANT_PID", pidPath)
	t.Setenv("NINEA_MCP_HELPER_READY", ready)
	t.Setenv("NINEA_MCP_HELPER_HANG", "1")
	adapter := New()
	p := mcpTestProvider(mcpHelperExecutable(t))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- adapter.Invoke(ctx, p, mcpTestCapability(), "invoke-context", json.RawMessage(`{}`), &mcpRecordingSink{})
	}()
	waitForMCPFile(t, ready)
	waitForMCPFile(t, pidPath)
	descendant := readMCPPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Invoke error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Invoke did not return after context cancellation")
	}
	waitForMCPProcessGone(t, descendant)
}

func TestCloseTerminatesMCPProcessTree(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	pidPath := filepath.Join(t.TempDir(), "descendant-pid")
	t.Setenv(mcpHelperEnv, "server")
	t.Setenv("NINEA_MCP_HELPER_DESCENDANT", "1")
	t.Setenv("NINEA_MCP_HELPER_DESCENDANT_PID", pidPath)
	t.Setenv("NINEA_MCP_HELPER_READY", ready)
	t.Setenv("NINEA_MCP_HELPER_HANG", "1")
	adapter := New()
	p := mcpTestProvider(mcpHelperExecutable(t))
	invokeCtx, cancelInvoke := context.WithCancel(context.Background())
	defer cancelInvoke()
	done := make(chan error, 1)
	go func() {
		done <- adapter.Invoke(invokeCtx, p, mcpTestCapability(), "invoke-close", json.RawMessage(`{}`), &mcpRecordingSink{})
	}()
	waitForMCPFile(t, ready)
	waitForMCPFile(t, pidPath)
	descendant := readMCPPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })
	closeCtx, cancelClose := context.WithTimeout(context.Background(), time.Second)
	defer cancelClose()
	if err := adapter.Close(closeCtx, p); err != nil {
		t.Fatalf("Close error=%v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Invoke did not return after Close")
	}
	waitForMCPProcessGone(t, descendant)
}

func TestCancelTerminatesMCPProcessTreeAndReturnsCanceled(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	pidPath := filepath.Join(t.TempDir(), "descendant-pid")
	t.Setenv(mcpHelperEnv, "server")
	t.Setenv("NINEA_MCP_HELPER_DESCENDANT", "1")
	t.Setenv("NINEA_MCP_HELPER_DESCENDANT_PID", pidPath)
	t.Setenv("NINEA_MCP_HELPER_READY", ready)
	t.Setenv("NINEA_MCP_HELPER_HANG", "1")
	adapter := New()
	p := mcpTestProvider(mcpHelperExecutable(t))
	invokeCtx, cancelInvoke := context.WithCancel(context.Background())
	defer cancelInvoke()
	done := make(chan error, 1)
	go func() {
		done <- adapter.Invoke(invokeCtx, p, mcpTestCapability(), "invoke-cancel", json.RawMessage(`{}`), &mcpRecordingSink{})
	}()
	waitForMCPFile(t, ready)
	waitForMCPFile(t, pidPath)
	descendant := readMCPPID(t, pidPath)
	t.Cleanup(func() { _ = syscall.Kill(descendant, syscall.SIGKILL) })
	cancelCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := adapter.Cancel(cancelCtx, p, "invoke-cancel"); err != nil {
		t.Fatalf("Cancel error=%v", err)
	}
	select {
	case err := <-done:
		var adapterErr *provider.AdapterError
		if !errors.As(err, &adapterErr) || adapterErr.Code() != "canceled" {
			t.Fatalf("Invoke error=%T %v", err, err)
		}
	case <-time.After(time.Second):
		t.Fatal("Invoke did not return after Cancel")
	}
	waitForMCPProcessGone(t, descendant)
}

func TestDirectMCPServerExitAndNormalStdioReturn(t *testing.T) {
	// The race runtime otherwise delays subprocess exit for one second. Keep a
	// wider deadline because this test also starts two race-instrumented helper
	// processes and can run alongside the rest of the suite.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	t.Setenv(mcpHelperEnv, "server")
	adapter := New()
	p := mcpTestProvider(mcpHelperExecutable(t))
	sink := &mcpRecordingSink{}
	if err := adapter.Invoke(context.Background(), p, mcpTestCapability(), "invoke-normal", json.RawMessage(`{}`), sink); err != nil || len(sink.events) != 1 {
		t.Fatalf("normal Invoke events=%#v error=%v", sink.events, err)
	}
	t.Setenv("NINEA_MCP_HELPER_EXIT", "1")
	p = mcpTestProvider(mcpHelperExecutable(t))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adapter.Invoke(ctx, p, mcpTestCapability(), "invoke-exit", json.RawMessage(`{}`), &mcpRecordingSink{}); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("direct exit error=%v", err)
	}
}

func TestMCPRejectsRelativeAndPATHExecutableEndpoints(t *testing.T) {
	adapter := New()
	for _, endpoint := range []string{"stdio:mcp-server", "stdio:relative/server"} {
		provider := provider.Provider{ID: "mcp/test", Protocol: "mcp", Name: "test", Endpoint: endpoint}
		if _, err := adapter.Discover(context.Background(), provider); err == nil || !strings.Contains(err.Error(), "absolute") {
			t.Fatalf("endpoint %q error=%v", endpoint, err)
		}
	}
}

func TestMCPGlobalSessionAdmissionAllows64AndRejectsBeforeSpawn(t *testing.T) {
	spawnPath := filepath.Join(t.TempDir(), "spawned")
	readyPath := filepath.Join(t.TempDir(), "ready")
	adapter := New()
	p := mcpTestProvider(mcpShellExecutable(t, spawnPath, readyPath, true))
	done := make(chan error, maxActiveMCPSessions)
	for i := 0; i < maxActiveMCPSessions; i++ {
		go func(i int) {
			done <- adapter.Invoke(context.Background(), p, mcpTestCapability(), fmt.Sprintf("admission-%d", i), json.RawMessage(`{}`), &mcpRecordingSink{})
		}(i)
	}
	waitForMCPLineCount(t, readyPath, maxActiveMCPSessions)
	if got := mcpActiveSessionCount(adapter); got != maxActiveMCPSessions {
		t.Fatalf("active sessions=%d want=%d", got, maxActiveMCPSessions)
	}
	overflowErr := adapter.Invoke(context.Background(), p, mcpTestCapability(), "admission-overflow", json.RawMessage(`{}`), &mcpRecordingSink{})
	var adapterErr *provider.AdapterError
	if !errors.As(overflowErr, &adapterErr) || adapterErr.Code() != "resource_exhausted" {
		t.Fatalf("overflow error=%T %v", overflowErr, overflowErr)
	}
	data, err := os.ReadFile(spawnPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(data, []byte{'\n'}); got != maxActiveMCPSessions {
		t.Fatalf("spawn count=%d want=%d", got, maxActiveMCPSessions)
	}
	closeCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := adapter.Close(closeCtx, p); err != nil {
		t.Fatalf("Close error=%v", err)
	}
	for i := 0; i < maxActiveMCPSessions; i++ {
		<-done
	}
	if got := mcpActiveSessionCount(adapter); got != 0 {
		t.Fatalf("active sessions after Close=%d", got)
	}
}

func TestMCPAdmissionReleasesStartFailureNormalCancelAndClose(t *testing.T) {
	t.Run("start failure", func(t *testing.T) {
		adapter := New()
		adapter.maxActiveSessions = 1
		bad := mcpTestProvider(filepath.Join(t.TempDir(), "missing"))
		if err := adapter.Invoke(context.Background(), bad, mcpTestCapability(), "start-failure", json.RawMessage(`{}`), &mcpRecordingSink{}); err == nil {
			t.Fatal("missing executable started")
		}
		if got := mcpActiveSessionCount(adapter); got != 0 {
			t.Fatalf("active sessions=%d", got)
		}
	})

	t.Run("normal", func(t *testing.T) {
		spawnPath := filepath.Join(t.TempDir(), "spawned")
		adapter := New()
		adapter.maxActiveSessions = 1
		p := mcpTestProvider(mcpShellExecutable(t, spawnPath, "", false))
		if err := adapter.Invoke(context.Background(), p, mcpTestCapability(), "normal-release", json.RawMessage(`{}`), &mcpRecordingSink{}); err != nil {
			t.Fatal(err)
		}
		if got := mcpActiveSessionCount(adapter); got != 0 {
			t.Fatalf("active sessions=%d", got)
		}
	})

	for _, operation := range []string{"cancel", "close"} {
		t.Run(operation, func(t *testing.T) {
			spawnPath := filepath.Join(t.TempDir(), "spawned")
			readyPath := filepath.Join(t.TempDir(), "ready")
			adapter := New()
			adapter.maxActiveSessions = 1
			p := mcpTestProvider(mcpShellExecutable(t, spawnPath, readyPath, true))
			done := make(chan error, 1)
			go func() {
				done <- adapter.Invoke(context.Background(), p, mcpTestCapability(), operation+"-release", json.RawMessage(`{}`), &mcpRecordingSink{})
			}()
			waitForMCPLineCount(t, readyPath, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var err error
			if operation == "cancel" {
				err = adapter.Cancel(ctx, p, operation+"-release")
			} else {
				err = adapter.Close(ctx, p)
			}
			if err != nil {
				t.Fatalf("%s error=%v", operation, err)
			}
			<-done
			if got := mcpActiveSessionCount(adapter); got != 0 {
				t.Fatalf("active sessions=%d", got)
			}
		})
	}
}

func TestMCPConcurrentCancelAndCloseDoNotDoubleReleaseAdmission(t *testing.T) {
	spawnPath := filepath.Join(t.TempDir(), "spawned")
	readyPath := filepath.Join(t.TempDir(), "ready")
	adapter := New()
	adapter.maxActiveSessions = 1
	p := mcpTestProvider(mcpShellExecutable(t, spawnPath, readyPath, true))
	done := make(chan error, 1)
	go func() {
		done <- adapter.Invoke(context.Background(), p, mcpTestCapability(), "release-race", json.RawMessage(`{}`), &mcpRecordingSink{})
	}()
	waitForMCPLineCount(t, readyPath, 1)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_ = adapter.Cancel(context.Background(), p, "release-race")
		}()
		go func() {
			defer wg.Done()
			_ = adapter.Close(context.Background(), p)
		}()
	}
	wg.Wait()
	<-done
	if got := mcpActiveSessionCount(adapter); got != 0 {
		t.Fatalf("active sessions=%d", got)
	}
}

func TestSplitTerminatedMCPResponseLineAcceptsLFCRLFAndBoundary(t *testing.T) {
	for _, test := range []struct {
		name      string
		data      []byte
		wantToken int
	}{
		{name: "LF", data: []byte("{}\n"), wantToken: 2},
		{name: "CRLF", data: []byte("{}\r\n"), wantToken: 2},
		{name: "exact boundary", data: append(bytes.Repeat([]byte{'x'}, maxResponseLineBytes-1), '\n'), wantToken: maxResponseLineBytes - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			advance, token, err := splitTerminatedMCPResponseLine(test.data, false)
			if err != nil || advance != len(test.data) || len(token) != test.wantToken {
				t.Fatalf("advance=%d token=%d error=%v", advance, len(token), err)
			}
		})
	}
}

func TestMCPRejectsUnterminatedResponseAtEOFAndAcceptsCRLF(t *testing.T) {
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	t.Setenv(mcpHelperEnv, "server")
	t.Run("unterminated", func(t *testing.T) {
		t.Setenv("NINEA_MCP_HELPER_UNTERMINATED", "1")
		p := mcpTestProvider(mcpHelperExecutable(t))
		_, err := New().Discover(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "newline") {
			t.Fatalf("unterminated response error=%v", err)
		}
	})
	t.Run("CRLF", func(t *testing.T) {
		t.Setenv("NINEA_MCP_HELPER_CRLF", "1")
		p := mcpTestProvider(mcpHelperExecutable(t))
		caps, err := New().Discover(context.Background(), p)
		if err != nil || len(caps) != 1 {
			t.Fatalf("CRLF capabilities=%d error=%v", len(caps), err)
		}
	})
}

func TestDiscoveryAlwaysRequiresApproval(t *testing.T) {
	t.Setenv(mcpHelperEnv, "server")
	for _, test := range []struct {
		name     string
		readOnly string
		want     string
	}{
		{name: "unspecified side effects", want: "always"},
		{name: "server claims read only", readOnly: "1", want: "always"},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("NINEA_MCP_HELPER_READ_ONLY", test.readOnly)
			p := mcpTestProvider(mcpHelperExecutable(t))
			capabilities, err := New().Discover(context.Background(), p)
			if err != nil || len(capabilities) != 1 {
				t.Fatalf("capabilities=%#v error=%v", capabilities, err)
			}
			if capabilities[0].Security.RequiresApproval != test.want {
				t.Fatalf("requires approval=%q want %q", capabilities[0].Security.RequiresApproval, test.want)
			}
		})
	}
}

func TestDiscoveryPreservesLargeIntegersInSchemas(t *testing.T) {
	t.Setenv(mcpHelperEnv, "server")
	t.Setenv(mcpHelperLargeSchemaEnv, "1")
	capabilities, err := New().Discover(context.Background(), mcpTestProvider(mcpHelperExecutable(t)))
	if err != nil || len(capabilities) != 1 {
		t.Fatalf("capabilities=%#v error=%v", capabilities, err)
	}
	input, err := json.Marshal(capabilities[0].Input.JSONSchema)
	if err != nil {
		t.Fatal(err)
	}
	output, err := json.Marshal(capabilities[0].Output.JSONSchema)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{"input": input, "output": output} {
		if !bytes.Contains(raw, []byte(`9007199254740993`)) {
			t.Fatalf("%s schema lost integer precision: %s", name, raw)
		}
	}
}

func TestInvokePreservesLargeIntegerArguments(t *testing.T) {
	t.Setenv(mcpHelperEnv, "server")
	capture := filepath.Join(t.TempDir(), "params.json")
	t.Setenv(mcpHelperCaptureParamsEnv, capture)

	err := New().Invoke(
		context.Background(),
		mcpTestProvider(mcpHelperExecutable(t)),
		mcpTestCapability(),
		"large-integer",
		json.RawMessage(`{"id":9007199254740993}`),
		&mcpRecordingSink{},
	)
	if err != nil {
		t.Fatal(err)
	}
	params, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(params, []byte(`"id":9007199254740993`)) {
		t.Fatalf("MCP parameters lost integer precision: %s", params)
	}
}

func TestDiscoveryRejectsEmptyTools(t *testing.T) {
	t.Setenv(mcpHelperEnv, "server")
	t.Setenv("NINEA_MCP_HELPER_EMPTY_TOOLS", "1")
	p := mcpTestProvider(mcpHelperExecutable(t))
	_, err := New().Discover(context.Background(), p)
	if err == nil || !strings.Contains(err.Error(), "no tools") {
		t.Fatalf("empty tools error=%v", err)
	}
}

func TestInvokeMapsToolResultErrorToRunFailure(t *testing.T) {
	t.Setenv(mcpHelperEnv, "server")
	t.Setenv("NINEA_MCP_HELPER_TOOL_ERROR", "1")
	adapter := New()
	p := mcpTestProvider(mcpHelperExecutable(t))
	capability := mcpTestCapability()
	capability.Output.JSONSchema = mcpTestOutputSchema()
	err := adapter.Invoke(context.Background(), p, capability, "tool-error", json.RawMessage(`{}`), &mcpRecordingSink{})
	var adapterErr *provider.AdapterError
	if !errors.As(err, &adapterErr) || adapterErr.Code() != "tool_error" || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("Invoke error=%T %v", err, err)
	}
}

func TestParseCallToolResult(t *testing.T) {
	tests := []struct {
		name        string
		result      string
		wantError   bool
		wantIsError bool
	}{
		{name: "null", result: `null`, wantError: true},
		{name: "empty object", result: `{}`, wantError: true},
		{name: "missing content with structured content", result: `{"structuredContent":{"answer":"ok"}}`, wantError: true},
		{name: "null content", result: `{"content":null}`, wantError: true},
		{name: "non-array content", result: `{"content":{}}`, wantError: true},
		{name: "non-object content block", result: `{"content":[null]}`, wantError: true},
		{name: "content block missing type", result: `{"content":[{}]}`, wantError: true},
		{name: "text block missing text", result: `{"content":[{"type":"text"}]}`, wantError: true},
		{name: "image block missing MIME type", result: `{"content":[{"type":"image","data":"aA=="}]}`, wantError: true},
		{name: "resource link missing name", result: `{"content":[{"type":"resource_link","uri":"file:///a"}]}`, wantError: true},
		{name: "embedded resource missing data", result: `{"content":[{"type":"resource","resource":{"uri":"file:///a"}}]}`, wantError: true},
		{name: "embedded resource has text and blob", result: `{"content":[{"type":"resource","resource":{"uri":"file:///a","text":"a","blob":"YQ=="}}]}`, wantError: true},
		{name: "invalid isError type", result: `{"content":[],"isError":"true"}`, wantError: true},
		{name: "null isError", result: `{"content":[],"isError":null}`, wantError: true},
		{name: "invalid structured content type", result: `{"content":[],"structuredContent":[]}`, wantError: true},
		{name: "empty content", result: `{"content":[]}`},
		{name: "text content", result: `{"content":[{"type":"text","text":""}]}`},
		{name: "image content", result: `{"content":[{"type":"image","data":"","mimeType":"image/png"}]}`},
		{name: "audio content", result: `{"content":[{"type":"audio","data":"","mimeType":"audio/wav"}]}`},
		{name: "resource link", result: `{"content":[{"type":"resource_link","name":"a","uri":"file:///a"}]}`},
		{name: "embedded text resource", result: `{"content":[{"type":"resource","resource":{"uri":"file:///a","text":""}}]}`},
		{name: "embedded blob resource", result: `{"content":[{"type":"resource","resource":{"uri":"file:///a","blob":""}}]}`},
		{name: "structured content", result: `{"content":[],"structuredContent":{}}`},
		{name: "tool error", result: `{"content":[],"isError":true}`, wantIsError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			isError, err := parseCallToolResult(json.RawMessage(test.result))
			if (err != nil) != test.wantError {
				t.Fatalf("parseCallToolResult error=%v wantError=%v", err, test.wantError)
			}
			if isError != test.wantIsError {
				t.Fatalf("parseCallToolResult isError=%v want=%v", isError, test.wantIsError)
			}
		})
	}
}

func TestInvokeValidatesToolOutputSchemaBeforeEmittingResult(t *testing.T) {
	t.Setenv(mcpHelperEnv, "server")
	tests := []struct {
		name      string
		result    string
		wantError bool
	}{
		{name: "missing structured content", result: `{"content":[]}`, wantError: true},
		{name: "schema mismatch", result: `{"content":[],"structuredContent":{"answer":1}}`, wantError: true},
		{name: "valid", result: `{"content":[],"structuredContent":{"answer":"ok"}}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(mcpHelperToolResultEnv, test.result)
			capability := mcpTestCapability()
			capability.Output.JSONSchema = mcpTestOutputSchema()
			sink := &mcpRecordingSink{}
			err := New().Invoke(context.Background(), mcpTestProvider(mcpHelperExecutable(t)), capability, "output-validation", json.RawMessage(`{}`), sink)
			if (err != nil) != test.wantError {
				t.Fatalf("Invoke error=%v wantError=%v", err, test.wantError)
			}
			if test.wantError && (!errors.Is(err, errInvalidMCPToolResult) || len(sink.events) != 0) {
				t.Fatalf("invalid result error=%v events=%#v", err, sink.events)
			}
			if !test.wantError && len(sink.events) != 1 {
				t.Fatalf("valid result events=%#v", sink.events)
			}
		})
	}
}

func mcpTestOutputSchema() map[string]any {
	return map[string]any{
		"type":     "object",
		"required": []any{"structuredContent"},
		"properties": map[string]any{
			"structuredContent": map[string]any{
				"type":     "object",
				"required": []any{"answer"},
				"properties": map[string]any{
					"answer": map[string]any{"type": "string"},
				},
			},
		},
	}
}

func TestSafeEnvironmentUsesMinimalAllowlist(t *testing.T) {
	input := []string{
		"PATH=/usr/bin", "HOME=/tmp/home", "LANG=en_US.UTF-8",
		"AWS_SECRET_ACCESS_KEY=secret", "GITHUB_TOKEN=secret", "NINEA_TOKEN=secret", "MALFORMED",
	}
	want := []string{"PATH=/usr/bin", "HOME=/tmp/home", "LANG=en_US.UTF-8"}
	if got := safeEnvironment(input); !reflect.DeepEqual(got, want) {
		t.Fatalf("safeEnvironment()=%q want %q", got, want)
	}
}

func TestMCPProcessUsesOwningWorkspaceAsWorkingDirectory(t *testing.T) {
	workspace := t.TempDir()
	marker := filepath.Join(t.TempDir(), "cwd")
	executable := filepath.Join(t.TempDir(), "mcp-cwd-helper")
	script := fmt.Sprintf(`#!/bin/sh
pwd > %q
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '%%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}' ;;
    *'"method":"tools/list"'*) printf '%%s\n' '{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo","inputSchema":{"type":"object"}}]}}' ;;
  esac
done
`, marker)
	if err := os.WriteFile(executable, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	p := mcpTestProvider(executable)
	p.Config = map[string]string{"workspace_root": workspace}
	if _, err := New().Discover(context.Background(), p); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != canonical {
		t.Fatalf("MCP cwd=%q want %q", strings.TrimSpace(string(data)), canonical)
	}
}

func TestSessionScannerKeepsStdoutReadableAfterProcessExit(t *testing.T) {
	t.Setenv(mcpHelperEnv, "server")
	t.Setenv("NINEA_MCP_HELPER_UNTERMINATED", "1")
	p := mcpTestProvider(mcpHelperExecutable(t))
	s, err := startSession(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.stop(context.Background()) }()
	s.callMu.Lock()
	defer s.callMu.Unlock()
	if err := json.NewEncoder(s.in).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	<-s.done
	if s.scan.Scan() {
		t.Fatal("unterminated response unexpectedly produced a token")
	}
	if !errors.Is(s.scan.Err(), errUnterminatedMCPResponse) {
		t.Fatalf("scanner error=%v", s.scan.Err())
	}
}

func TestSessionReapsDescendantHoldingStdoutAfterProcessExit(t *testing.T) {
	t.Setenv(mcpHelperEnv, "server")
	t.Setenv("NINEA_MCP_HELPER_UNTERMINATED", "1")
	t.Setenv("NINEA_MCP_HELPER_DESCENDANT", "1")
	p := mcpTestProvider(mcpHelperExecutable(t))
	s, err := startSession(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.stop(context.Background()) }()
	s.callMu.Lock()
	locked := true
	defer func() {
		if locked {
			s.callMu.Unlock()
		}
	}()
	if err := json.NewEncoder(s.in).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	<-s.done
	result := make(chan error, 1)
	go func() {
		if s.scan.Scan() {
			result <- errors.New("unterminated response unexpectedly produced a token")
			return
		}
		result <- s.scan.Err()
	}()
	select {
	case err := <-result:
		if !errors.Is(err, errUnterminatedMCPResponse) {
			t.Fatalf("scanner error=%v", err)
		}
	case <-time.After(time.Second):
		_ = processgroup.Kill(s.cmd)
		<-result
		s.callMu.Unlock()
		locked = false
		t.Fatal("scanner remained blocked by inherited stdout")
	}
}
