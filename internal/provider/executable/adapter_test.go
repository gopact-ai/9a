package executable

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
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
)

const helperEnv = "NINEA_EXECUTABLE_ADAPTER_HELPER"

type helperRequest struct {
	Version string          `json:"version"`
	ID      string          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func TestAdapterHelperProcess(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		return
	}
	if counter := os.Getenv("NINEA_EXECUTABLE_ADAPTER_COUNTER"); counter != "" {
		f, _ := os.OpenFile(counter, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if f != nil {
			_, _ = f.WriteString("start\n")
			_ = f.Close()
		}
	}
	if leak := os.Getenv("NINEA_EXECUTABLE_ADAPTER_LEAK"); leak != "" && (os.Getenv("NINEA_TOKEN") != "" || os.Getenv("NINEA_BOOTSTRAP_TOKEN") != "") {
		_ = os.WriteFile(leak, []byte("leaked"), 0600)
	}
	if os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") == "never-read" {
		if ready := os.Getenv("NINEA_EXECUTABLE_ADAPTER_READY"); ready != "" {
			_ = os.WriteFile(ready, []byte("ready"), 0600)
		}
		for {
			time.Sleep(time.Second)
		}
	}
	if os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") == "descendant-stdout" {
		child := exec.Command("sh", "-c", "while :; do sleep 1; done")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		if path := os.Getenv("NINEA_EXECUTABLE_ADAPTER_DESCENDANT_PID"); path != "" {
			_ = os.WriteFile(path, []byte(fmt.Sprint(child.Process.Pid)), 0600)
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes*2)
	encoder := json.NewEncoder(os.Stdout)
	var heldInvoke *helperRequest
	var sharedInvokeA, sharedInvokeB, heldHealth string
	for scanner.Scan() {
		var request helperRequest
		if err := json.Unmarshal(scanner.Bytes(), &request); err != nil {
			os.Exit(2)
		}
		result := any(nil)
		switch request.Method {
		case "discover":
			var params struct {
				Provider struct {
					Name     string `json:"name"`
					Endpoint string `json:"endpoint"`
				} `json:"provider"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if params.Provider.Name != "billing" || params.Provider.Endpoint != "https://billing.example.com" {
				writeHelperError(encoder, request.ID, "invalid_request", "provider params")
				continue
			}
			capability := map[string]any{
				"upstream_name": "create-invoice", "kind": "api.operation", "name": "Create invoice",
				"description": "Create a draft invoice", "input": map[string]any{"mode": "json", "json_schema": map[string]any{"type": "object"}},
				"output": map[string]any{"mode": "json"}, "lifecycle": map[string]any{"sync": true},
				"security": map[string]any{"requires_approval": "always", "upstream_auth": "adapter-configured"}, "tags": []string{"billing"},
			}
			if os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") == "multi-turn" {
				capability["lifecycle"] = map[string]any{"sync": true, "multi_turn": true}
			}
			capabilities := []any{capability}
			switch os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") {
			case "long-description":
				capability["description"] = strings.Repeat("d", 513)
			case "large-schema":
				capability["input"] = map[string]any{"mode": "json", "json_schema": map[string]any{"description": strings.Repeat("s", (1<<20)+1)}}
			case "too-many-capabilities":
				capabilities = make([]any, 10_001)
				for i := range capabilities {
					copy := map[string]any{}
					for key, value := range capability {
						copy[key] = value
					}
					copy["upstream_name"] = fmt.Sprintf("cap-%d", i)
					capabilities[i] = copy
				}
			case "missing-required":
				delete(capability, "description")
			}
			switch os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") {
			case "discover-missing-capabilities":
				result = map[string]any{}
			case "discover-null-capabilities":
				result = map[string]any{"capabilities": nil}
			case "discover-empty-capabilities":
				result = map[string]any{"capabilities": []any{}}
			default:
				result = map[string]any{"capabilities": capabilities}
			}
		case "health":
			if os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") == "shared-health" {
				if heldHealth == "" {
					heldHealth = request.ID
					if ready := os.Getenv("NINEA_EXECUTABLE_ADAPTER_READY"); ready != "" {
						_ = os.WriteFile(ready, []byte("ready"), 0600)
					}
					continue
				}
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": heldHealth, "result": map[string]any{"healthy": true, "message": "late"}})
				heldHealth = ""
			}
			if strings.HasPrefix(os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE"), "shared-invoke") {
				if sharedInvokeA != "" {
					_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": sharedInvokeA, "result": map[string]any{"output": map[string]any{"late": true}}})
					sharedInvokeA = ""
				}
				if sharedInvokeB != "" {
					_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": sharedInvokeB, "result": map[string]any{"output": map[string]any{"id": sharedInvokeB}}})
					sharedInvokeB = ""
				}
			}
			switch os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") {
			case "health-null":
				result = nil
			case "health-missing-healthy":
				result = map[string]any{"message": "missing"}
			case "health-false":
				result = map[string]any{"healthy": false, "message": "maintenance"}
			default:
				result = map[string]any{"healthy": true, "message": fmt.Sprintf("pid=%d", os.Getpid())}
			}
		case "invoke":
			mode := os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE")
			if mode == "cancel-abandon-context" || mode == "cancel-abandon-sink" {
				if strings.HasSuffix(request.ID, "-b") {
					_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "result": map[string]any{"output": map[string]any{"id": request.ID}}})
					continue
				}
				copy := request
				heldInvoke = &copy
				if mode == "cancel-abandon-sink" {
					_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "event": map[string]any{"sequence": 1, "type": "status", "data": map[string]any{"state": "waiting"}}})
				}
				if ready := os.Getenv("NINEA_EXECUTABLE_ADAPTER_READY"); ready != "" {
					_ = os.WriteFile(ready, []byte("ready"), 0600)
				}
				continue
			}
			switch os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") {
			case "shared-invoke-context", "shared-invoke-sink":
				if strings.HasSuffix(request.ID, "-a") {
					sharedInvokeA = request.ID
					_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "event": map[string]any{"sequence": 1, "type": "status", "data": map[string]any{"state": "waiting"}}})
					if ready := os.Getenv("NINEA_EXECUTABLE_ADAPTER_READY"); ready != "" {
						_ = os.WriteFile(ready, []byte("ready"), 0600)
					}
				} else {
					sharedInvokeB = request.ID
					if ready := os.Getenv("NINEA_EXECUTABLE_ADAPTER_READY_B"); ready != "" {
						_ = os.WriteFile(ready, []byte("ready"), 0600)
					}
				}
				continue
			case "error-empty-code":
				writeHelperError(encoder, request.ID, "", "safe message")
				continue
			case "error-empty-message":
				writeHelperError(encoder, request.ID, "upstream_error", "")
				continue
			case "error-typed":
				writeHelperError(encoder, request.ID, "upstream_error", "safe message")
				continue
			case "wrong-version":
				_ = encoder.Encode(map[string]any{"version": "9a.adapter/v999", "id": request.ID, "result": map[string]any{"output": map[string]any{}}})
				continue
			case "wrong-id":
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID + "-wrong", "result": map[string]any{"output": map[string]any{}}})
				continue
			case "malformed":
				_, _ = os.Stdout.WriteString("{not-json}\n")
				continue
			case "unterminated-response":
				message, _ := json.Marshal(map[string]any{"version": protocolVersion, "id": request.ID, "result": map[string]any{"output": map[string]any{"ok": true}}})
				_, _ = os.Stdout.Write(message)
				os.Exit(0)
			case "invalid-sequence":
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "event": map[string]any{"sequence": 2, "type": "status", "data": map[string]any{}}})
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "event": map[string]any{"sequence": 2, "type": "status", "data": map[string]any{}}})
				continue
			case "unsupported-encoding":
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "artifact": map[string]any{"sequence": 1, "name": "x", "media_type": "application/octet-stream", "encoding": "hex", "data": "00"}})
				continue
			case "oversized-response":
				_, _ = os.Stdout.WriteString(strings.Repeat("x", maxLineBytes+1) + "\n")
				continue
			case "child-exit":
				os.Exit(9)
			case "child-exit-with-pending":
				if heldInvoke == nil {
					copy := request
					heldInvoke = &copy
					continue
				}
				os.Exit(9)
			case "slow-sink":
				for sequence := 1; sequence <= 3; sequence++ {
					_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "event": map[string]any{"sequence": sequence, "type": "status", "data": map[string]any{"sequence": sequence}}})
				}
				continue
			case "duplicate-terminal":
				message := map[string]any{"version": protocolVersion, "id": request.ID, "result": map[string]any{"output": map[string]any{"ok": true}}}
				_ = encoder.Encode(message)
				_ = encoder.Encode(message)
				continue
			case "event-after-terminal":
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "result": map[string]any{"output": map[string]any{"ok": true}}})
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "event": map[string]any{"sequence": 1, "type": "status", "data": map[string]any{}}})
				continue
			case "hang":
				if ready := os.Getenv("NINEA_EXECUTABLE_ADAPTER_READY"); ready != "" {
					_ = os.WriteFile(ready, []byte("ready"), 0600)
				}
				continue
			case "observe-oversized-request":
				if marker := os.Getenv("NINEA_EXECUTABLE_ADAPTER_OVERSIZED_MARKER"); marker != "" {
					_ = os.WriteFile(marker, []byte("received"), 0600)
				}
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "result": map[string]any{"output": map[string]any{}}})
				continue
			}
			if os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") == "invalid-utf8" {
				_, _ = os.Stdout.Write([]byte(`{"version":"` + protocolVersion + `","id":"` + request.ID + `","result":{"output":{"value":"`))
				_, _ = os.Stdout.Write([]byte{0xff})
				_, _ = os.Stdout.Write([]byte("\"}}}\n"))
				continue
			}
			if strings.HasPrefix(os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE"), "cancel") {
				copy := request
				heldInvoke = &copy
				if ready := os.Getenv("NINEA_EXECUTABLE_ADAPTER_READY"); ready != "" {
					_ = os.WriteFile(ready, []byte("ready"), 0600)
				}
				continue
			}
			if os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") == "interleave" {
				if heldInvoke == nil {
					copy := request
					heldInvoke = &copy
					continue
				}
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": heldInvoke.ID, "event": map[string]any{"sequence": 1, "type": "status", "data": map[string]any{"id": heldInvoke.ID}}})
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "event": map[string]any{"sequence": 1, "type": "status", "data": map[string]any{"id": request.ID}}})
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "result": map[string]any{"output": map[string]any{"id": request.ID}}})
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": heldInvoke.ID, "result": map[string]any{"output": map[string]any{"id": heldInvoke.ID}}})
				heldInvoke = nil
				continue
			}
			var params struct {
				Provider   providerParams `json:"provider"`
				Capability struct {
					UpstreamName string `json:"upstream_name"`
				} `json:"capability"`
				Input map[string]any `json:"input"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if request.ID != "call-stable-1" || params.Provider.Name != "billing" || params.Capability.UpstreamName != "create-invoice" || params.Input["amount"] != float64(42) {
				writeHelperError(encoder, request.ID, "invalid_request", "invoke params")
				continue
			}
			_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "event": map[string]any{"sequence": 1, "type": "status", "data": map[string]any{"state": "working"}}})
			_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "artifact": map[string]any{"sequence": 2, "name": "invoice.txt", "media_type": "text/plain", "encoding": "base64", "data": base64.StdEncoding.EncodeToString([]byte("invoice"))}})
			result = map[string]any{"output": map[string]any{"invoice_id": "inv_123"}}
		case "cancel":
			var params struct {
				InvocationID string `json:"invocation_id"`
			}
			_ = json.Unmarshal(request.Params, &params)
			if heldInvoke == nil || params.InvocationID != heldInvoke.ID {
				writeHelperError(encoder, request.ID, "not_cancelable", "not active")
				continue
			}
			mode := os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE")
			if mode == "cancel-abandon-context" || mode == "cancel-abandon-sink" {
				if mode == "cancel-abandon-sink" {
					writeHelperError(encoder, heldInvoke.ID, "canceled", "invocation canceled")
				}
				if ready := os.Getenv("NINEA_EXECUTABLE_ADAPTER_CANCEL_READY"); ready != "" {
					_ = os.WriteFile(ready, []byte("ready"), 0600)
				}
				for {
					if _, err := os.Stat(os.Getenv("NINEA_EXECUTABLE_ADAPTER_CANCEL_RELEASE")); err == nil {
						break
					}
					time.Sleep(time.Millisecond)
				}
				if mode == "cancel-abandon-context" {
					writeHelperError(encoder, heldInvoke.ID, "canceled", "invocation canceled")
				}
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "result": map[string]any{"canceled": true}})
				heldInvoke = nil
				continue
			}
			if os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") == "cancel-race-not-cancelable" {
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": heldInvoke.ID, "result": map[string]any{"output": map[string]any{"completed": true}}})
				heldInvoke = nil
				writeHelperError(encoder, request.ID, "not_cancelable", "already completed")
				continue
			}
			_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "result": map[string]any{"canceled": true}})
			switch os.Getenv("NINEA_EXECUTABLE_ADAPTER_MODE") {
			case "cancel-dishonest-no-terminal":
				continue
			case "cancel-dishonest-success":
				_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": heldInvoke.ID, "result": map[string]any{"output": map[string]any{"unexpected": true}}})
				heldInvoke = nil
				continue
			}
			writeHelperError(encoder, heldInvoke.ID, "canceled", "invocation canceled")
			heldInvoke = nil
			continue
		default:
			writeHelperError(encoder, request.ID, "unsupported_method", request.Method)
			continue
		}
		_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": request.ID, "result": result, "ignored": true})
	}
	os.Exit(0)
}

func writeHelperError(encoder *json.Encoder, id, code, message string) {
	_ = encoder.Encode(map[string]any{"version": protocolVersion, "id": id, "error": map[string]any{"code": code, "message": message}})
}

func helperExecutable(t *testing.T) string {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "adapter-helper")
	script := fmt.Sprintf("#!/bin/sh\nexec %q -test.run=^TestAdapterHelperProcess$\n", binary)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDiscoverAndHealthReuseProviderProcess(t *testing.T) {
	ctx := context.Background()
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	providerConfig := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	caps, err := adapter.Discover(ctx, providerConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(caps) != 1 {
		t.Fatalf("capabilities=%#v", caps)
	}
	capability := caps[0]
	if capability.ID != "billing-api/billing/create-invoice" || capability.Source.Protocol != "billing-api" || capability.Source.Provider != "billing" || capability.Source.UpstreamName != "create-invoice" {
		t.Fatalf("derived capability=%#v", capability)
	}
	if capability.Input.Mode != "json" || capability.Input.JSONSchema["type"] != "object" || capability.Security.RequiresApproval != "always" {
		t.Fatalf("mapped capability=%#v", capability)
	}
	health := adapter.Health(ctx, providerConfig)
	if !health.Healthy || !strings.HasPrefix(health.Message, "pid=") {
		t.Fatalf("health=%#v", health)
	}
	starts, err := os.ReadFile(counter)
	if err != nil || strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("process starts=%q err=%v", starts, err)
	}
	if err := adapter.Close(ctx, providerConfig); err != nil {
		t.Fatal(err)
	}
}

func TestNewRejectsNonAbsoluteExecutable(t *testing.T) {
	if _, err := New("billing-api", "relative/adapter"); err == nil {
		t.Fatal("relative executable accepted")
	}
}

func TestDiscoverRejectsMultiTurnV1Capability(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "multi-turn")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	if _, err := adapter.Discover(context.Background(), p); err == nil || !strings.Contains(err.Error(), "multi_turn") {
		t.Fatalf("multi-turn discovery error=%v", err)
	}
}

func TestDiscoverRejectsCapabilityBoundsAndMissingFields(t *testing.T) {
	for _, mode := range []string{"long-description", "large-schema", "too-many-capabilities", "missing-required"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(helperEnv, "1")
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", mode)
			adapter, err := New("billing-api", helperExecutable(t))
			if err != nil {
				t.Fatal(err)
			}
			p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
			defer adapter.Close(context.Background(), p)
			if _, err := adapter.Discover(context.Background(), p); err == nil {
				t.Fatalf("mode %s accepted", mode)
			}
		})
	}
}

type recordingSink struct {
	order     []string
	events    []provider.Event
	artifacts [][]byte
}

type failingStartedSink struct{ err error }

func (s failingStartedSink) Started() error { return s.err }
func (s failingStartedSink) Event(provider.Event) error {
	return errors.New("event called after Started failure")
}
func (s failingStartedSink) Artifact(string, string, []byte) error {
	return errors.New("artifact called after Started failure")
}

func (*recordingSink) Started() error { return nil }
func (s *recordingSink) Event(event provider.Event) error {
	s.order = append(s.order, "event:"+event.Type)
	s.events = append(s.events, event)
	return nil
}

func (s *recordingSink) Artifact(name, mediaType string, data []byte) error {
	s.order = append(s.order, "artifact:"+name+":"+mediaType)
	s.artifacts = append(s.artifacts, append([]byte(nil), data...))
	return nil
}

func TestInvokeForwardsEventsArtifactAndTerminalOutput(t *testing.T) {
	t.Setenv(helperEnv, "1")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	var _ provider.Adapter = adapter
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	capability := capabilityForTest()
	sink := &recordingSink{}
	if err := adapter.Invoke(context.Background(), p, capability, "call-stable-1", json.RawMessage(`{"amount":42}`), sink); err != nil {
		t.Fatal(err)
	}
	wantOrder := []string{"event:status", "artifact:invoice.txt:text/plain", "event:result"}
	if fmt.Sprint(sink.order) != fmt.Sprint(wantOrder) {
		t.Fatalf("order=%v want=%v", sink.order, wantOrder)
	}
	if len(sink.artifacts) != 1 || !bytes.Equal(sink.artifacts[0], []byte("invoice")) {
		t.Fatalf("artifacts=%q", sink.artifacts)
	}
	if len(sink.events) != 2 || !bytes.Contains(sink.events[0].Data, []byte(`"working"`)) || string(sink.events[1].Data) != `{"invoice_id":"inv_123"}` {
		t.Fatalf("events=%#v", sink.events)
	}
}

func TestInvokeAbandonsWrittenRequestWhenStartedFails(t *testing.T) {
	t.Setenv(helperEnv, "1")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	want := errors.New("persist readiness failed")
	err = adapter.Invoke(context.Background(), p, capabilityForTest(), "call-started-fails", json.RawMessage(`{"amount":42}`), failingStartedSink{err: want})
	if !errors.Is(err, want) {
		t.Fatalf("Invoke() error=%v", err)
	}
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("session unusable after abandoned request: %#v", health)
	}
}

func capabilityForTest() capability.Capability {
	return capability.Capability{ID: "billing-api/billing/create-invoice", Kind: "api.operation", Name: "Create invoice", Description: "Create invoice", Source: capability.Source{Protocol: "billing-api", Provider: "billing", UpstreamName: "create-invoice"}, Input: capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "json"}}
}

func TestCancelUsesActiveInvocationIDAndCompletesOriginalAsCanceled(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "cancel")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", ready)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	invokeErr := make(chan error, 1)
	go func() {
		invokeErr <- adapter.Invoke(context.Background(), p, capabilityForTest(), "call-cancel-me", json.RawMessage(`{"amount":42}`), &recordingSink{})
	}()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := adapter.Cancel(context.Background(), p, "call-cancel-me"); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	select {
	case err := <-invokeErr:
		if err == nil || !strings.Contains(err.Error(), "canceled") {
			t.Fatalf("invoke error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("invoke did not finish after cancellation")
	}
}

func TestConcurrentInvocationsMayInterleaveByID(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "interleave")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	type outcome struct {
		id   string
		sink *recordingSink
		err  error
	}
	outcomes := make(chan outcome, 2)
	for _, id := range []string{"call-a", "call-b"} {
		id := id
		go func() {
			sink := &recordingSink{}
			err := adapter.Invoke(context.Background(), p, capabilityForTest(), id, json.RawMessage(`{"amount":42}`), sink)
			outcomes <- outcome{id: id, sink: sink, err: err}
		}()
	}
	for range 2 {
		outcome := <-outcomes
		if outcome.err != nil {
			t.Fatalf("%s: %v", outcome.id, outcome.err)
		}
		if len(outcome.sink.events) != 2 || !bytes.Contains(outcome.sink.events[0].Data, []byte(outcome.id)) || !bytes.Contains(outcome.sink.events[1].Data, []byte(outcome.id)) {
			t.Fatalf("%s events=%#v", outcome.id, outcome.sink.events)
		}
	}
}

func TestInvalidUTF8ProtocolMessageFails(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "invalid-utf8")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	err = adapter.Invoke(context.Background(), p, capabilityForTest(), "call-stable-1", json.RawMessage(`{"amount":42}`), &recordingSink{})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "utf-8") {
		t.Fatalf("invalid UTF-8 error=%v", err)
	}
}

func TestSplitTerminatedResponseLineAcceptsCompleteAndExactLimitLines(t *testing.T) {
	for _, test := range []struct {
		name      string
		data      []byte
		wantToken int
	}{
		{name: "complete", data: []byte("{}\n"), wantToken: 2},
		{name: "exact limit", data: append(bytes.Repeat([]byte{'x'}, maxLineBytes-1), '\n'), wantToken: maxLineBytes - 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			advance, token, err := splitTerminatedResponseLine(test.data, false)
			if err != nil || advance != len(test.data) || len(token) != test.wantToken {
				t.Fatalf("advance=%d token=%d error=%v", advance, len(token), err)
			}
		})
	}
}

func TestTerminalAdapterErrorIsMachineClassifiable(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "error-typed")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	err = adapter.Invoke(context.Background(), p, capabilityForTest(), "call-stable-1", json.RawMessage(`{"amount":42}`), &recordingSink{})
	var typed *provider.AdapterError
	if !errors.As(err, &typed) || typed.Code() != "upstream_error" || typed.Message() != "safe message" || err.Error() != "upstream_error: safe message" {
		t.Fatalf("Invoke error=%T %v typed=%#v", err, err, typed)
	}
}

func TestProtocolViolationsAndChildExitUnblockInvoke(t *testing.T) {
	// The helper re-executes this test binary. Disable the race runtime's exit
	// delay in that child so a loaded full-suite run measures adapter teardown,
	// not race-process shutdown latency.
	t.Setenv("GORACE", strings.TrimSpace(os.Getenv("GORACE")+" atexit_sleep_ms=0"))
	for _, mode := range []string{"wrong-version", "wrong-id", "malformed", "unterminated-response", "invalid-sequence", "unsupported-encoding", "oversized-response", "child-exit"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(helperEnv, "1")
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", mode)
			adapter, err := New("billing-api", helperExecutable(t))
			if err != nil {
				t.Fatal(err)
			}
			p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
			defer adapter.Close(context.Background(), p)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err = adapter.Invoke(ctx, p, capabilityForTest(), "call-stable-1", json.RawMessage(`{"amount":42}`), &recordingSink{})
			if err == nil {
				t.Fatalf("mode %s succeeded", mode)
			}
			if errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("mode %s did not unblock before deadline", mode)
			}
		})
	}
}

func TestPostTerminalMessagesInvalidateOnlyThatProviderSession(t *testing.T) {
	for _, mode := range []string{"duplicate-terminal", "event-after-terminal"} {
		t.Run(mode, func(t *testing.T) {
			counter := filepath.Join(t.TempDir(), "starts")
			t.Setenv(helperEnv, "1")
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", mode)
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
			adapter, err := New("billing-api", helperExecutable(t))
			if err != nil {
				t.Fatal(err)
			}
			p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
			defer adapter.Close(context.Background(), p)
			_ = adapter.Invoke(context.Background(), p, capabilityForTest(), "call-stable-1", json.RawMessage(`{"amount":42}`), &recordingSink{})
			var health provider.Health
			for i := 0; i < 100; i++ {
				health = adapter.Health(context.Background(), p)
				starts, _ := os.ReadFile(counter)
				if health.Healthy && strings.Count(string(starts), "start\n") >= 2 {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("session was not invalidated; health=%#v", health)
		})
	}
}

func TestContextCancellationReleasesCallerWithoutRestartingProcess(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "starts")
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "hang")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", ready)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	ctx, cancel := context.WithCancel(context.Background())
	invokeErr := make(chan error, 1)
	go func() {
		invokeErr <- adapter.Invoke(ctx, p, capabilityForTest(), "call-hang", json.RawMessage(`{"amount":42}`), &recordingSink{})
	}()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err = <-invokeErr
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke error=%v", err)
	}
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("health after restart=%#v", health)
	}
	starts, _ := os.ReadFile(counter)
	if strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("process starts=%q", starts)
	}
}

func TestChildEnvironmentStripsDaemonTokens(t *testing.T) {
	leak := filepath.Join(t.TempDir(), "leak")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_LEAK", leak)
	t.Setenv("NINEA_TOKEN", "agent-secret")
	t.Setenv("NINEA_BOOTSTRAP_TOKEN", "admin-secret")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("health=%#v", health)
	}
	if _, err := os.Stat(leak); !os.IsNotExist(err) {
		t.Fatalf("daemon token leaked to child: %v", err)
	}
}

func TestCloseTerminatesOnlySelectedProviderSession(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p1 := provider.Provider{ID: "billing-api/one", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	p2 := provider.Provider{ID: "billing-api/two", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	if !adapter.Health(context.Background(), p1).Healthy || !adapter.Health(context.Background(), p2).Healthy {
		t.Fatal("initial health failed")
	}
	if err := adapter.Close(context.Background(), p1); err != nil {
		t.Fatal(err)
	}
	if !adapter.Health(context.Background(), p2).Healthy || !adapter.Health(context.Background(), p1).Healthy {
		t.Fatal("health after close failed")
	}
	_ = adapter.Close(context.Background(), p1)
	_ = adapter.Close(context.Background(), p2)
	starts, _ := os.ReadFile(counter)
	if strings.Count(string(starts), "start\n") != 3 {
		t.Fatalf("process starts=%q", starts)
	}
}

func TestOversizedRequestIsRejectedBeforeChildReceivesIt(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "received")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "observe-oversized-request")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_OVERSIZED_MARKER", marker)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	input, _ := json.Marshal(map[string]string{"value": strings.Repeat("x", maxLineBytes)})
	err = adapter.Invoke(context.Background(), p, capabilityForTest(), "call-too-large", input, &recordingSink{})
	if err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized request error=%v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("oversized request reached child: %v", err)
	}
}

func TestEncodeRequestLineIncludesNewlineInExactLimit(t *testing.T) {
	base := request{Version: protocolVersion, ID: "boundary", Method: "invoke", Params: json.RawMessage(`""`)}
	baseEncoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	padding := maxLineBytes - 1 - len(baseEncoded)
	base.Params = json.RawMessage(`"` + strings.Repeat("x", padding) + `"`)
	line, err := encodeRequestLine(base)
	if err != nil || len(line) != maxLineBytes || line[len(line)-1] != '\n' {
		t.Fatalf("exact line bytes=%d error=%v", len(line), err)
	}
	base.Params = json.RawMessage(`"` + strings.Repeat("x", padding+1) + `"`)
	if _, err := encodeRequestLine(base); err == nil || !strings.Contains(err.Error(), "limit") {
		t.Fatalf("oversized total line error=%v", err)
	}
}

func TestUnsupportedArtifactEncodingInvalidatesSession(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "unsupported-encoding")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	err = adapter.Invoke(context.Background(), p, capabilityForTest(), "call-stable-1", json.RawMessage(`{"amount":42}`), &recordingSink{})
	if err == nil {
		t.Fatal("unsupported encoding succeeded")
	}
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("health after protocol failure=%#v", health)
	}
	starts, _ := os.ReadFile(counter)
	if strings.Count(string(starts), "start\n") != 2 {
		t.Fatalf("protocol-failed session was reused; starts=%q", starts)
	}
}

func TestChildExitUnblocksAllConcurrentPendingCalls(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "child-exit-with-pending")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	errorsOut := make(chan error, 2)
	for _, id := range []string{"call-pending-a", "call-pending-b"} {
		id := id
		go func() {
			errorsOut <- adapter.Invoke(context.Background(), p, capabilityForTest(), id, json.RawMessage(`{"amount":42}`), &recordingSink{})
		}()
	}
	for range 2 {
		select {
		case err := <-errorsOut:
			if err == nil {
				t.Fatal("pending invocation succeeded after child exit")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("pending invocation remained blocked after child exit")
		}
	}
}

func TestCanceledBlockedWriteKillsSessionAndReturns(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "never-read")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", ready)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	ctx, cancel := context.WithCancel(context.Background())
	invokeErr := make(chan error, 1)
	input, _ := json.Marshal(map[string]string{"value": strings.Repeat("x", 1<<20)})
	go func() {
		invokeErr <- adapter.Invoke(ctx, p, capabilityForTest(), "call-blocked-write", input, &recordingSink{})
	}()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-invokeErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Invoke error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked adapter write ignored context cancellation")
	}
}

type blockingSink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (*blockingSink) Started() error { return nil }
func (s *blockingSink) Event(provider.Event) error {
	s.once.Do(func() { close(s.entered) })
	<-s.release
	return nil
}

func (*blockingSink) Artifact(string, string, []byte) error { return nil }

func TestCloseCannotDeadlockOnFullPendingEventChannel(t *testing.T) {
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "slow-sink")
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	sink := &blockingSink{entered: make(chan struct{}), release: make(chan struct{})}
	invokeErr := make(chan error, 1)
	go func() {
		invokeErr <- adapter.Invoke(context.Background(), p, capabilityForTest(), "call-slow-sink", json.RawMessage(`{"amount":42}`), sink)
	}()
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("sink was not called")
	}
	closeErr := make(chan error, 1)
	go func() { closeErr <- adapter.Close(context.Background(), p) }()
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		close(sink.release)
		<-closeErr
		<-invokeErr
		t.Fatal("Close deadlocked on a full pending event channel")
	}
	close(sink.release)
	select {
	case err := <-invokeErr:
		if err == nil {
			t.Fatal("invoke succeeded after session close")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("invoke remained blocked after session close")
	}
}

func TestErrorEnvelopeRequiresCodeAndMessage(t *testing.T) {
	for _, mode := range []string{"error-empty-code", "error-empty-message"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(helperEnv, "1")
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", mode)
			adapter, err := New("billing-api", helperExecutable(t))
			if err != nil {
				t.Fatal(err)
			}
			p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
			defer adapter.Close(context.Background(), p)
			err = adapter.Invoke(context.Background(), p, capabilityForTest(), "call-bad-error", json.RawMessage(`{"amount":42}`), &recordingSink{})
			if err == nil || !strings.Contains(err.Error(), "invalid adapter error") {
				t.Fatalf("mode %s error=%v", mode, err)
			}
		})
	}
}

func TestDiscoverRequiresPresentNonNullCapabilitiesArray(t *testing.T) {
	for _, mode := range []string{"discover-missing-capabilities", "discover-null-capabilities"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(helperEnv, "1")
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", mode)
			adapter, err := New("billing-api", helperExecutable(t))
			if err != nil {
				t.Fatal(err)
			}
			p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
			defer adapter.Close(context.Background(), p)
			if _, err := adapter.Discover(context.Background(), p); err == nil {
				t.Fatalf("mode %s accepted", mode)
			}
		})
	}
	t.Run("explicit-empty", func(t *testing.T) {
		t.Setenv(helperEnv, "1")
		t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "discover-empty-capabilities")
		adapter, err := New("billing-api", helperExecutable(t))
		if err != nil {
			t.Fatal(err)
		}
		p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
		defer adapter.Close(context.Background(), p)
		caps, err := adapter.Discover(context.Background(), p)
		if err != nil || caps == nil || len(caps) != 0 {
			t.Fatalf("explicit empty capabilities=%#v err=%v", caps, err)
		}
	})
}

func TestHealthRequiresPresentBoolean(t *testing.T) {
	for _, mode := range []string{"health-null", "health-missing-healthy"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(helperEnv, "1")
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", mode)
			adapter, err := New("billing-api", helperExecutable(t))
			if err != nil {
				t.Fatal(err)
			}
			p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
			defer adapter.Close(context.Background(), p)
			health := adapter.Health(context.Background(), p)
			if health.Healthy || !strings.Contains(health.Message, "invalid health result") {
				t.Fatalf("mode %s health=%#v", mode, health)
			}
		})
	}
	t.Run("explicit-false", func(t *testing.T) {
		t.Setenv(helperEnv, "1")
		t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "health-false")
		adapter, err := New("billing-api", helperExecutable(t))
		if err != nil {
			t.Fatal(err)
		}
		p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
		defer adapter.Close(context.Background(), p)
		health := adapter.Health(context.Background(), p)
		if health.Healthy || health.Message != "maintenance" {
			t.Fatalf("health=%#v", health)
		}
	})
}

func TestCancelRejectsInactiveInvocationWithoutStartingProcess(t *testing.T) {
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	if err := adapter.Cancel(context.Background(), p, "call-not-active"); err == nil || !strings.Contains(err.Error(), "not_cancelable") {
		t.Fatalf("Cancel error=%v", err)
	}
	if _, err := os.Stat(counter); !os.IsNotExist(err) {
		t.Fatalf("inactive cancel started adapter process: %v", err)
	}
}

func TestDishonestCancelFailsSessionAndUnblocksOriginal(t *testing.T) {
	for _, mode := range []string{"cancel-dishonest-no-terminal", "cancel-dishonest-success"} {
		t.Run(mode, func(t *testing.T) {
			ready := filepath.Join(t.TempDir(), "ready")
			t.Setenv(helperEnv, "1")
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", mode)
			t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", ready)
			adapter, err := New("billing-api", helperExecutable(t))
			if err != nil {
				t.Fatal(err)
			}
			p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
			defer adapter.Close(context.Background(), p)
			invokeErr := make(chan error, 1)
			go func() {
				invokeErr <- adapter.Invoke(context.Background(), p, capabilityForTest(), "call-cancel-lie", json.RawMessage(`{"amount":42}`), &recordingSink{})
			}()
			for i := 0; i < 100; i++ {
				if _, err := os.Stat(ready); err == nil {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
			err = adapter.Cancel(ctx, p, "call-cancel-lie")
			cancel()
			if err == nil {
				t.Fatal("dishonest cancellation succeeded")
			}
			select {
			case err := <-invokeErr:
				if err == nil {
					t.Fatal("original invocation succeeded after dishonest cancel")
				}
			case <-time.After(2 * time.Second):
				t.Fatal("original invocation remained blocked after dishonest cancel")
			}
		})
	}
}

func TestCancelRaceNotCancelablePreservesOriginalResult(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "cancel-race-not-cancelable")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", ready)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	invokeErr := make(chan error, 1)
	go func() {
		invokeErr <- adapter.Invoke(context.Background(), p, capabilityForTest(), "call-race", json.RawMessage(`{"amount":42}`), &recordingSink{})
	}()
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	err = adapter.Cancel(context.Background(), p, "call-race")
	if err == nil || !strings.Contains(err.Error(), "not_cancelable") {
		t.Fatalf("Cancel error=%v", err)
	}
	select {
	case err := <-invokeErr:
		if err != nil {
			t.Fatalf("original invocation error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("original invocation did not finish")
	}
}

func TestCloseKillsDescendantHoldingStdoutAndHonorsContext(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "descendant.pid")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "descendant-stdout")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_DESCENDANT_PID", pidFile)
	adapter, err := New("billing-api", helperExecutable(t))
	if err != nil {
		t.Fatal(err)
	}
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("health=%#v", health)
	}
	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	var descendantPID int
	if _, err := fmt.Sscan(string(pidBytes), &descendantPID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	closeResult := make(chan error, 1)
	go func() { closeResult <- adapter.Close(ctx, p) }()
	select {
	case err := <-closeResult:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		_ = syscall.Kill(descendantPID, syscall.SIGKILL)
		<-closeResult
		t.Fatal("Close hung on descendant-inherited stdout")
	}
	for i := 0; i < 100; i++ {
		if executableTestProcessTerminated(descendantPID) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived Close", descendantPID)
}

func executableTestProcessTerminated(pid int) bool {
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

func waitForFile(t *testing.T, path string) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestInvokeContextCancellationDoesNotKillSharedSession(t *testing.T) {
	readyA := filepath.Join(t.TempDir(), "ready-a")
	readyB := filepath.Join(t.TempDir(), "ready-b")
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "shared-invoke-context")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", readyA)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY_B", readyB)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, _ := New("billing-api", helperExecutable(t))
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	ctxA, cancelA := context.WithCancel(context.Background())
	errA := make(chan error, 1)
	go func() {
		errA <- adapter.Invoke(ctxA, p, capabilityForTest(), "call-shared-a", json.RawMessage(`{"amount":1}`), &recordingSink{})
	}()
	waitForFile(t, readyA)
	errB := make(chan error, 1)
	go func() {
		errB <- adapter.Invoke(context.Background(), p, capabilityForTest(), "call-shared-b", json.RawMessage(`{"amount":2}`), &recordingSink{})
	}()
	waitForFile(t, readyB)
	cancelA()
	if err := <-errA; !errors.Is(err, context.Canceled) {
		t.Fatalf("A error=%v", err)
	}
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("health=%#v", health)
	}
	select {
	case err := <-errB:
		if err != nil {
			t.Fatalf("B error=%v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("B remained blocked")
	}
	starts, _ := os.ReadFile(counter)
	if strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("shared session restarted: %q", starts)
	}
}

type failingSink struct{ err error }

func (s failingSink) Started() error                        { return nil }
func (s failingSink) Event(provider.Event) error            { return s.err }
func (s failingSink) Artifact(string, string, []byte) error { return s.err }

type blockingFailingSink struct {
	err     error
	entered chan struct{}
	release chan struct{}
}

func (s blockingFailingSink) Started() error { return nil }
func (s blockingFailingSink) Event(provider.Event) error {
	close(s.entered)
	<-s.release
	return s.err
}

func (s blockingFailingSink) Artifact(string, string, []byte) error { return s.err }

func TestCancelThenContextAbandonAcceptsCanceledTerminalWithoutKillingSession(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "invoke-ready")
	cancelReady := filepath.Join(t.TempDir(), "cancel-ready")
	cancelRelease := filepath.Join(t.TempDir(), "cancel-release")
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "cancel-abandon-context")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", ready)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_CANCEL_READY", cancelReady)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_CANCEL_RELEASE", cancelRelease)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, _ := New("billing-api", helperExecutable(t))
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)

	ctxA, cancelA := context.WithCancel(context.Background())
	invokeErr := make(chan error, 1)
	go func() {
		invokeErr <- adapter.Invoke(ctxA, p, capabilityForTest(), "call-cancel-context-a", json.RawMessage(`{"amount":1}`), &recordingSink{})
	}()
	waitForFile(t, ready)
	cancelErr := make(chan error, 1)
	go func() { cancelErr <- adapter.Cancel(context.Background(), p, "call-cancel-context-a") }()
	waitForFile(t, cancelReady)
	cancelA()
	if err := <-invokeErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("Invoke A error=%v", err)
	}
	if err := os.WriteFile(cancelRelease, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-cancelErr; err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if err := adapter.Invoke(context.Background(), p, capabilityForTest(), "call-cancel-context-b", json.RawMessage(`{"amount":2}`), &recordingSink{}); err != nil {
		t.Fatalf("Invoke B: %v", err)
	}
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("health=%#v", health)
	}
	starts, _ := os.ReadFile(counter)
	if strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("shared session restarted: %q", starts)
	}
}

func TestHeldCanceledTerminalThenSinkAbandonRestoresAccounting(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "invoke-ready")
	cancelReady := filepath.Join(t.TempDir(), "cancel-ready")
	cancelRelease := filepath.Join(t.TempDir(), "cancel-release")
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "cancel-abandon-sink")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", ready)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_CANCEL_READY", cancelReady)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_CANCEL_RELEASE", cancelRelease)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, _ := New("billing-api", helperExecutable(t))
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)

	sinkErr := errors.New("sink failed")
	sink := blockingFailingSink{err: sinkErr, entered: make(chan struct{}), release: make(chan struct{})}
	invokeErr := make(chan error, 1)
	go func() {
		invokeErr <- adapter.Invoke(context.Background(), p, capabilityForTest(), "call-cancel-sink-a", json.RawMessage(`{"amount":1}`), sink)
	}()
	waitForFile(t, ready)
	select {
	case <-sink.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("sink did not receive event")
	}
	cancelErr := make(chan error, 1)
	go func() { cancelErr <- adapter.Cancel(context.Background(), p, "call-cancel-sink-a") }()
	waitForFile(t, cancelReady)
	s := adapter.existingSession(p)
	deadline := time.Now().Add(2 * time.Second)
	for {
		s.mu.Lock()
		pending := s.pending["call-cancel-sink-a"]
		held := pending != nil && pending.heldTerminal != nil
		s.mu.Unlock()
		if held {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("canceled terminal was not held before sink abandonment")
		}
		time.Sleep(time.Millisecond)
	}
	close(sink.release)
	if err := <-invokeErr; !errors.Is(err, sinkErr) {
		t.Fatalf("Invoke A error=%v", err)
	}
	if err := os.WriteFile(cancelRelease, []byte("release"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-cancelErr; err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	s.mu.Lock()
	abandoned := s.abandoned
	_, stillPending := s.pending["call-cancel-sink-a"]
	s.mu.Unlock()
	if abandoned != 0 || stillPending {
		t.Fatalf("abandonment accounting not restored: abandoned=%d pending=%v", abandoned, stillPending)
	}
	if err := adapter.Invoke(context.Background(), p, capabilityForTest(), "call-cancel-sink-b", json.RawMessage(`{"amount":2}`), &recordingSink{}); err != nil {
		t.Fatalf("Invoke B: %v", err)
	}
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("health=%#v", health)
	}
	starts, _ := os.ReadFile(counter)
	if strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("shared session restarted: %q", starts)
	}
}

func TestSinkFailureDoesNotKillSharedSession(t *testing.T) {
	readyA := filepath.Join(t.TempDir(), "ready-a")
	readyB := filepath.Join(t.TempDir(), "ready-b")
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "shared-invoke-sink")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", readyA)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY_B", readyB)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, _ := New("billing-api", helperExecutable(t))
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	sinkErr := errors.New("sink failed")
	errA := make(chan error, 1)
	go func() {
		errA <- adapter.Invoke(context.Background(), p, capabilityForTest(), "call-sink-a", json.RawMessage(`{"amount":1}`), failingSink{err: sinkErr})
	}()
	waitForFile(t, readyA)
	if err := <-errA; !errors.Is(err, sinkErr) {
		t.Fatalf("A error=%v", err)
	}
	errB := make(chan error, 1)
	go func() {
		errB <- adapter.Invoke(context.Background(), p, capabilityForTest(), "call-sink-b", json.RawMessage(`{"amount":2}`), &recordingSink{})
	}()
	waitForFile(t, readyB)
	if health := adapter.Health(context.Background(), p); !health.Healthy {
		t.Fatalf("health=%#v", health)
	}
	if err := <-errB; err != nil {
		t.Fatalf("B error=%v", err)
	}
	starts, _ := os.ReadFile(counter)
	if strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("shared session restarted: %q", starts)
	}
}

func TestNonInvokeContextCancellationDoesNotKillSharedSession(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	counter := filepath.Join(t.TempDir(), "starts")
	t.Setenv(helperEnv, "1")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_MODE", "shared-health")
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_READY", ready)
	t.Setenv("NINEA_EXECUTABLE_ADAPTER_COUNTER", counter)
	adapter, _ := New("billing-api", helperExecutable(t))
	p := provider.Provider{ID: "billing-api/billing", Protocol: "billing-api", Name: "billing", Endpoint: "https://billing.example.com"}
	defer adapter.Close(context.Background(), p)
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan provider.Health, 1)
	go func() { first <- adapter.Health(ctx, p) }()
	waitForFile(t, ready)
	cancel()
	if health := <-first; !strings.Contains(health.Message, context.Canceled.Error()) {
		t.Fatalf("canceled health=%#v", health)
	}
	secondCtx, secondCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer secondCancel()
	if health := adapter.Health(secondCtx, p); !health.Healthy {
		t.Fatalf("second health=%#v", health)
	}
	starts, _ := os.ReadFile(counter)
	if strings.Count(string(starts), "start\n") != 1 {
		t.Fatalf("shared session restarted: %q", starts)
	}
}

func TestPendingAndAbandonedRequestBounds(t *testing.T) {
	t.Run("pending", func(t *testing.T) {
		s := &session{pending: make(map[string]*pendingRequest, maxPendingRequests), stopped: make(chan struct{}), writeGate: make(chan struct{}, 1)}
		for i := 0; i < maxPendingRequests; i++ {
			s.pending[fmt.Sprint(i)] = &pendingRequest{}
		}
		if _, err := s.begin(context.Background(), "overflow", "health", map[string]any{}, false); err == nil {
			t.Fatal("pending request bound not enforced")
		}
	})
	t.Run("abandoned", func(t *testing.T) {
		s := &session{pending: map[string]*pendingRequest{}, abandoned: maxAbandonedRequests, stopped: make(chan struct{})}
		s.pending["overflow"] = &pendingRequest{abandonedCh: make(chan struct{})}
		s.abandon("overflow")
		if !s.isDead() || !strings.Contains(s.failure().Error(), "too many abandoned") {
			t.Fatalf("abandoned request bound not enforced: %v", s.failure())
		}
	})
}

func TestSessionFailureClearsAbandonedAccounting(t *testing.T) {
	s := &session{
		pending: map[string]*pendingRequest{
			"abandoned": {abandoned: true},
		},
		abandoned: 1,
		stopped:   make(chan struct{}),
	}
	s.fail(errors.New("failed"))
	if s.abandoned != 0 || len(s.pending) != 0 {
		t.Fatalf("failed session retained abandoned accounting: abandoned=%d pending=%d", s.abandoned, len(s.pending))
	}
}

func TestCancellationWhileWaitingForWriteGateDoesNotKillSession(t *testing.T) {
	s := &session{pending: map[string]*pendingRequest{}, stopped: make(chan struct{}), writeGate: make(chan struct{}, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.begin(ctx, "not-written", "health", map[string]any{}, false); !errors.Is(err, context.Canceled) {
		t.Fatalf("begin error=%v", err)
	}
	if s.isDead() {
		t.Fatal("cancellation before writing killed the shared session")
	}
	if len(s.pending) != 0 {
		t.Fatalf("unsent request remains pending: %d", len(s.pending))
	}
}
