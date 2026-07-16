package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/jsoncontract"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/search"
	"github.com/gopact-ai/9a/internal/store"
)

type protocolTestAdapter struct {
	discovered []provider.Provider
	closed     []provider.Provider
}

func (a *protocolTestAdapter) Discover(_ context.Context, p provider.Provider) ([]capability.Capability, error) {
	a.discovered = append(a.discovered, p)
	return []capability.Capability{{
		ID:          capability.StableID(p.Protocol, p.Name, "echo"),
		Kind:        p.Protocol + ".operation",
		Name:        "echo",
		Description: "Echo input",
		Source:      capability.Source{Protocol: p.Protocol, Provider: p.Name, UpstreamName: "echo"},
		Input:       capability.Contract{Mode: "json", JSONSchema: map[string]any{"type": "object"}},
		Output:      capability.Contract{Mode: "json"},
		Lifecycle:   capability.Lifecycle{Sync: true},
		Security:    capability.Security{RequiresApproval: "always", UpstreamAuth: "none"},
	}}, nil
}

func (*protocolTestAdapter) Invoke(_ context.Context, _ provider.Provider, _ capability.Capability, _ string, input json.RawMessage, sink provider.Sink) error {
	if err := sink.Started(); err != nil {
		return err
	}
	return sink.Event(provider.Event{Type: "result", Data: input})
}

func (*protocolTestAdapter) Cancel(context.Context, provider.Provider, string) error { return nil }

func (*protocolTestAdapter) Health(context.Context, provider.Provider) provider.Health {
	return provider.Health{Healthy: true}
}

func (a *protocolTestAdapter) Close(_ context.Context, p provider.Provider) error {
	a.closed = append(a.closed, p)
	return nil
}

func TestMCPAndA2AUseTheIntegrationLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	adapter := &protocolTestAdapter{}
	a.mu.Lock()
	a.adapters["mcp"] = adapter
	a.adapters["a2a"] = adapter
	a.mu.Unlock()
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	executable := filepath.Join(t.TempDir(), "mcp-server")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	canonicalExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}
	mcpSource := []byte("version: 1\nname: local-tools\ntype: mcp\nexecutable: " + executable + "\n")
	result, err := a.Connect(ctx, "admin", mcpSource, root)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "local-tools" || len(result.Capabilities) != 1 || result.Capabilities[0] != "local-tools/echo" {
		t.Fatalf("MCP connect result=%#v", result)
	}
	p, err := a.integrationByName(ctx, root, "local-tools")
	canonicalRoot, canonicalRootErr := filepath.EvalSymlinks(root)
	if canonicalRootErr != nil {
		t.Fatal(canonicalRootErr)
	}
	if err != nil || p == nil || p.Protocol != "mcp" || p.Endpoint != "stdio:"+canonicalExecutable || p.Config["workspace_root"] != canonicalRoot {
		t.Fatalf("MCP provider=%#v err=%v", p, err)
	}
	status, err := a.Status(ctx, root, "local-tools")
	if err != nil || status.State != "ready" || len(status.Integrations) != 1 {
		t.Fatalf("MCP status=%#v err=%v", status, err)
	}
	otherRoot := t.TempDir()
	if found, err := a.Search(ctx, "admin", otherRoot, search.Query{Text: "echo"}); err != nil || len(found) != 0 {
		t.Fatalf("cross-workspace search=%#v err=%v", found, err)
	}
	if _, err := a.RunInWorkspace(ctx, "admin", otherRoot, "local-tools/echo", json.RawMessage(`{"ok":true}`), ""); err == nil {
		t.Fatal("cross-workspace run was allowed")
	}
	if err := a.DisconnectFromWorkspace(ctx, "admin", otherRoot, "local-tools"); err == nil {
		t.Fatal("cross-workspace disconnect was allowed")
	}
	if found, err := a.Search(ctx, "admin", root, search.Query{Text: "local-tools/echo"}); err != nil || len(found) != 1 || found[0].Ref != "local-tools/echo" || found[0].Input == nil {
		t.Fatalf("exact workspace search=%#v err=%v", found, err)
	}
	if found, err := a.Search(ctx, "admin", root, search.Query{Text: "local-tools"}); err != nil || len(found) != 1 || found[0].Ref != "local-tools/echo" || found[0].Input != nil {
		t.Fatalf("integration workspace search=%#v err=%v", found, err)
	}
	input := json.RawMessage(`{"ok":true}`)
	if _, err := a.RunInWorkspace(ctx, "admin", root, "local-tools/echo", input, ""); err == nil {
		t.Fatal("MCP run bypassed approval")
	}
	approval := approvalForRun(t, a, "admin", root, "local-tools/echo", input)
	output, err := a.RunInWorkspace(ctx, "admin", root, "local-tools/echo", input, approval)
	if err != nil || string(output) != `{"ok":true}` {
		t.Fatalf("MCP run=%s err=%v", output, err)
	}

	a2aSource := []byte("version: 1\nname: research-agent\ntype: a2a\nurl: https://agent.example.com\n")
	result, err = a.Connect(ctx, "admin", a2aSource, root)
	if err != nil || len(result.Capabilities) != 1 || !strings.HasPrefix(result.Capabilities[0], "research-agent/") {
		t.Fatalf("A2A connect result=%#v err=%v", result, err)
	}
	if err := a.DisconnectFromWorkspace(ctx, "admin", root, "research-agent"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".9a", "integrations", "research-agent.yaml")); err != nil {
		t.Fatalf("disconnect removed canonical source: %v", err)
	}
	if remaining, err := a.integrationByName(ctx, root, "research-agent"); err != nil || remaining != nil {
		t.Fatalf("disconnected provider=%#v err=%v", remaining, err)
	}
}

func TestIntegrationNamesAreScopedToWorkspace(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	adapter := &protocolTestAdapter{}
	a.mu.Lock()
	a.adapters["a2a"] = adapter
	a.mu.Unlock()
	firstRoot := t.TempDir()
	secondRoot := t.TempDir()
	cleanupReadOnlyProjection(t, firstRoot)
	cleanupReadOnlyProjection(t, secondRoot)
	first := []byte("version: 1\nname: shared\ntype: a2a\nurl: https://first.example.com\n")
	if _, err := a.Connect(ctx, "admin", first, firstRoot); err != nil {
		t.Fatal(err)
	}
	second := []byte("version: 1\nname: shared\ntype: a2a\nurl: https://second.example.com\n")
	if _, err := a.Connect(ctx, "admin", second, secondRoot); err != nil {
		t.Fatalf("connect same name in second workspace: %v", err)
	}
	firstProvider, err := a.integrationByName(ctx, firstRoot, "shared")
	if err != nil || firstProvider == nil || firstProvider.Endpoint != "https://first.example.com" {
		t.Fatalf("first workspace provider=%#v err=%v", firstProvider, err)
	}
	secondProvider, err := a.integrationByName(ctx, secondRoot, "shared")
	if err != nil || secondProvider == nil || secondProvider.Endpoint != "https://second.example.com" || secondProvider.ID == firstProvider.ID {
		t.Fatalf("second workspace provider=%#v err=%v", secondProvider, err)
	}
	for _, root := range []string{firstRoot, secondRoot} {
		found, err := a.Search(ctx, "admin", root, search.Query{Text: "shared/echo"})
		if err != nil || len(found) != 1 || found[0].Ref != "shared/echo" {
			t.Fatalf("workspace %s search=%#v err=%v", root, found, err)
		}
	}
	if err := a.DisconnectFromWorkspace(ctx, "admin", firstRoot, "shared"); err != nil {
		t.Fatal(err)
	}
	if remaining, err := a.integrationByName(ctx, secondRoot, "shared"); err != nil || remaining == nil {
		t.Fatalf("disconnect affected second workspace: provider=%#v err=%v", remaining, err)
	}
}

func TestA2AInputSchemaRejectsEmptyPartBeforeCreatingCall(t *testing.T) {
	ctx := context.Background()
	var sends atomic.Int32
	var agent *httptest.Server
	agent = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/agent-card.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "Research Agent", "description": "Researches supplied material.", "version": "1.0.0",
				"supportedInterfaces": []any{map[string]any{"url": agent.URL + "/a2a/v1", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0"}},
				"capabilities":        map[string]any{"streaming": false},
				"defaultInputModes":   []string{"text/plain"}, "defaultOutputModes": []string{"application/json"},
				"skills": []any{map[string]any{"id": "summarize", "name": "Summarize", "description": "Summarize material.", "tags": []string{"summary"}}},
			})
		case "/a2a/v1/message:send":
			sends.Add(1)
			http.Error(w, "must not be called", http.StatusInternalServerError)
		default:
			http.NotFound(w, r)
		}
	}))
	defer agent.Close()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := New(db)
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	if _, err := a.Connect(ctx, "admin", []byte("version: 1\nname: research-agent\ntype: a2a\nurl: "+agent.URL+"\n"), root); err != nil {
		t.Fatal(err)
	}

	_, err = a.RunInWorkspace(ctx, "admin", root, "research-agent/summarize", json.RawMessage(`{"parts":[{}]}`), "")
	if !errors.Is(err, jsoncontract.ErrInvalidValue) {
		t.Fatalf("RunInWorkspace() error=%v", err)
	}
	var calls int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM calls`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if calls != 0 || sends.Load() != 0 {
		t.Fatalf("invalid A2A input created calls=%d sends=%d", calls, sends.Load())
	}
}

func TestRestoredA2AMissingCredentialStopsBeforeDiscovery(t *testing.T) {
	ctx := context.Background()
	var requests atomic.Int32
	var agent *httptest.Server
	agent = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/.well-known/agent-card.json" {
			http.Error(w, "must not be called", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "Private Agent", "description": "Handles private material.", "version": "1.0.0",
			"supportedInterfaces": []any{map[string]any{"url": agent.URL + "/a2a/v1", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0"}},
			"capabilities":        map[string]any{"streaming": false},
			"defaultInputModes":   []string{"text/plain"}, "defaultOutputModes": []string{"application/json"},
			"securitySchemes": map[string]any{
				"bearer": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}},
			},
			"securityRequirements": []any{map[string]any{"schemes": map[string]any{"bearer": map[string]any{"list": []any{}}}}},
			"skills":               []any{map[string]any{"id": "summarize", "name": "Summarize", "description": "Summarize material.", "tags": []string{"summary"}}},
		})
	}))
	defer agent.Close()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	backend := &memorySecretBackend{values: map[string]string{}}
	a := NewWithSecretBackend(db, backend)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	source := []byte("version: 1\nname: private-agent\ntype: a2a\nurl: " + agent.URL + "\ncredentials:\n  bearer:\n    secret: private-agent.bearer\n")
	if _, err := a.Connect(ctx, "admin", source, root); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 {
		t.Fatalf("connect requests=%d", requests.Load())
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}

	a = NewWithSecretBackend(db, backend)
	defer func() { _ = a.Close(context.Background()) }()
	if err := a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"parts":[{"text":"summarize"}]}`)
	approval := approvalForRun(t, a, "admin", root, "private-agent/summarize", input)
	_, err = a.RunInWorkspace(ctx, "admin", root, "private-agent/summarize", input, approval)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.Code != "missing_credential" || runErr.Credential != "private-agent.bearer" || runErr.SideEffect != "none" || runErr.CallID != "" {
		t.Fatalf("run error=%#v", err)
	}
	var calls int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM calls`).Scan(&calls); err != nil {
		t.Fatal(err)
	}
	if requests.Load() != 1 || calls != 0 {
		t.Fatalf("missing credential performed I/O: requests=%d calls=%d", requests.Load(), calls)
	}
}

func TestProtocolIntegrationsRestoreFromCanonicalSources(t *testing.T) {
	ctx := context.Background()
	database := filepath.Join(t.TempDir(), "ninea.db")
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	mcpServer := filepath.Join(t.TempDir(), "mcp-server")
	mcpStarts := filepath.Join(t.TempDir(), "mcp-starts")
	script := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf 'x\n' >> "MCP_STARTS"; printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}' ;;
    *'"method":"tools/list"'*) printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo","description":"Echo input","inputSchema":{"type":"object"}}]}}' ;;
    *'"method":"tools/call"'*) printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"ok"}]}}' ;;
  esac
done
`
	script = strings.Replace(script, "MCP_STARTS", mcpStarts, 1)
	if err := os.WriteFile(mcpServer, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	var discoveries atomic.Int32
	var agent *httptest.Server
	agent = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/agent-card.json" {
			http.NotFound(w, r)
			return
		}
		discoveries.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "Research Agent", "description": "Researches supplied material.", "version": "1.0.0",
			"supportedInterfaces": []any{map[string]any{"url": agent.URL + "/a2a/v1", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0"}},
			"capabilities":        map[string]any{"streaming": false},
			"defaultInputModes":   []string{"text/plain"}, "defaultOutputModes": []string{"application/json"},
			"skills": []any{map[string]any{"id": "summarize", "name": "Summarize", "description": "Summarize material.", "tags": []string{"summary"}}},
		})
	}))

	db, err := store.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	a := New(db)
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Connect(ctx, "admin", []byte("version: 1\nname: local-tools\ntype: mcp\nexecutable: "+mcpServer+"\n"), root); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Connect(ctx, "admin", []byte("version: 1\nname: research-agent\ntype: a2a\nurl: "+agent.URL+"\n"), root); err != nil {
		t.Fatal(err)
	}
	startsBeforeRestore, err := os.ReadFile(mcpStarts)
	if err != nil || strings.Count(string(startsBeforeRestore), "x\n") != 1 || discoveries.Load() != 1 {
		t.Fatalf("connect protocol activity: starts=%q discoveries=%d err=%v", startsBeforeRestore, discoveries.Load(), err)
	}
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	agent.Close()

	db, err = store.Open(ctx, database)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	a = New(db)
	defer func() { _ = a.Close(context.Background()) }()
	if err := a.Restore(ctx); err != nil {
		t.Fatal(err)
	}
	startsAfterRestore, err := os.ReadFile(mcpStarts)
	if err != nil || strings.Count(string(startsAfterRestore), "x\n") != 1 || discoveries.Load() != 1 {
		t.Fatalf("restore performed protocol I/O: starts=%q discoveries=%d err=%v", startsAfterRestore, discoveries.Load(), err)
	}
	status, err := a.Status(ctx, root, "")
	if err != nil || status.State != "ready" || len(status.Integrations) != 2 {
		t.Fatalf("restored status=%#v err=%v", status, err)
	}
	input := json.RawMessage(`{}`)
	approval := approvalForRun(t, a, "admin", root, "local-tools/echo", input)
	result, err := a.RunInWorkspace(ctx, "admin", root, "local-tools/echo", input, approval)
	if err != nil || !strings.Contains(string(result), `"text":"ok"`) {
		t.Fatalf("restored MCP run=%s err=%v", result, err)
	}
}
