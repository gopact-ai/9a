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
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
)

const mcpHelperEnv = "NINEA_MCP_HELPER"

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
			writeResponse(map[string]any{"jsonrpc": "2.0", "id": *request.ID, "result": map[string]any{"tools": []any{map[string]any{"name": "echo", "description": "Echo input.", "inputSchema": map[string]any{"type": "object"}}}}})
		case "tools/call":
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
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestMCPHelperProcess$\n", binary)
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func waitForMCPFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
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
	// The race runtime otherwise delays subprocess exit for one second, which
	// collides with this test's one-second context deadline.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	t.Setenv(mcpHelperEnv, "server")
	adapter := New()
	p := mcpTestProvider(mcpHelperExecutable(t))
	sink := &mcpRecordingSink{}
	if err := adapter.Invoke(context.Background(), p, mcpTestCapability(), "invoke-normal", json.RawMessage(`{}`), sink); err != nil || len(sink.events) != 1 {
		t.Fatalf("normal Invoke events=%#v error=%v", sink.events, err)
	}
	t.Setenv("NINEA_MCP_HELPER_EXIT", "1")
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
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
	p := mcpTestProvider(mcpHelperExecutable(t))
	t.Run("unterminated", func(t *testing.T) {
		t.Setenv("NINEA_MCP_HELPER_UNTERMINATED", "1")
		_, err := New().Discover(context.Background(), p)
		if err == nil || !strings.Contains(err.Error(), "newline") {
			t.Fatalf("unterminated response error=%v", err)
		}
	})
	t.Run("CRLF", func(t *testing.T) {
		t.Setenv("NINEA_MCP_HELPER_CRLF", "1")
		caps, err := New().Discover(context.Background(), p)
		if err != nil || len(caps) != 1 {
			t.Fatalf("CRLF capabilities=%d error=%v", len(caps), err)
		}
	})
}
