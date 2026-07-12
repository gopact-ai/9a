package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
)

func TestHTTPAdapterDiscoveryInvokeAsyncAuthAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("process e2e")
	}
	var mu sync.Mutex
	privateCalls, publicCalls := 0, 0
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("health Authorization=%q", got)
			}
			_, _ = w.Write([]byte(`{"healthy":true}`))
		case "/private":
			if got := r.Header.Get("Authorization"); got != "Bearer provider-http-secret" {
				t.Errorf("private Authorization=%q", got)
			}
			mu.Lock()
			privateCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"visibility": "private", "id": r.URL.Query().Get("id")})
		case "/public":
			if got := r.Header.Get("Authorization"); got != "" {
				t.Errorf("public Authorization=%q", got)
			}
			var input map[string]any
			_ = json.NewDecoder(r.Body).Decode(&input)
			mu.Lock()
			publicCalls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"visibility": "public", "echo": input["message"]})
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	cli, daemon, adapter := filepath.Join(bin, "9a"), filepath.Join(bin, "ninead"), filepath.Join(bin, "http-adapter")
	build(t, cli, "./cmd/9a")
	build(t, daemon, "./cmd/ninead")
	build(t, adapter, "./examples/http-adapter")
	manifestPath := filepath.Join(root, "manifest.json")
	manifest := map[string]any{
		"version": "1", "health_path": "/healthz", "health_auth": "none",
		"operations": []any{
			map[string]any{
				"upstream_name": "get-secret", "name": "Get secret", "description": "Returns private API data.", "method": "GET", "path": "/private",
				"input_schema": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string"}}}, "output_schema": map[string]any{"type": "object"},
				"tags": []any{"private", "read"}, "examples": []any{"Get record 123"}, "auth": "bearer", "requires_approval": "never",
			},
			map[string]any{
				"upstream_name": "echo", "name": "Echo", "description": "Returns public API data.", "method": "POST", "path": "/public",
				"input_schema": map[string]any{"type": "object"}, "output_schema": map[string]any{"type": "object"},
				"tags": []any{"public"}, "auth": "none", "requires_approval": "never",
			},
		},
	}
	data, _ := json.Marshal(manifest)
	if err := os.WriteFile(manifestPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	invalidManifest := filepath.Join(root, "invalid-manifest.json")
	if err := os.WriteFile(invalidManifest, []byte(`{"version":"2","operations":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := exec.Command(adapter)
	invalid.Env = append(os.Environ(), "NINEA_HTTP_ADAPTER_MANIFEST="+invalidManifest)
	if output, err := invalid.CombinedOutput(); err == nil || bytes.Contains(output, []byte("provider-http-secret")) {
		t.Fatalf("invalid manifest error=%v output=%s", err, output)
	}

	socket := socketPath(t)
	state := filepath.Join(root, "state.db")
	adminToken := "http-e2e-admin"
	adminEnv := append(os.Environ(), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+adminToken, "NINEA_HTTP_ADAPTER_MANIFEST="+manifestPath, "NINEA_HTTP_TOKEN_HTTP_API=provider-http-secret")
	var logs bytes.Buffer
	startDaemon := func(bootstrap bool) *exec.Cmd {
		command := exec.Command(daemon, "--state", state, "--socket", socket)
		command.Env = adminEnv
		if bootstrap {
			command.Env = append(command.Env, "NINEA_BOOTSTRAP_TOKEN="+adminToken)
		}
		command.Stderr = &logs
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		waitSocket(t, socket, &logs)
		return command
	}
	d := startDaemon(true)
	t.Cleanup(func() { _ = d.Process.Kill(); _ = d.Wait() })

	agentToken := strings.TrimSpace(string(run(t, adminEnv, cli, "", "tokens", "create", "agent")))
	agentEnv := append(os.Environ(), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+agentToken)
	if output := runFails(t, agentEnv, cli, "", "adapters", "add", "denied-http", adapter); !bytes.Contains(output, []byte("permission_denied")) {
		t.Fatalf("non-admin adapter registration=%s", output)
	}
	run(t, adminEnv, cli, "", "adapters", "add", "httpapi", adapter)
	if output := runFails(t, adminEnv, cli, "", "providers", "add", "httpapi", "bad-endpoint", "http://api.example"); !bytes.Contains(output, []byte("invalid_request")) {
		t.Fatalf("invalid endpoint=%s", output)
	}
	run(t, adminEnv, cli, "", "providers", "add", "httpapi", "http-api", api.URL)
	for _, capability := range []string{"httpapi/http-api/get-secret", "httpapi/http-api/echo"} {
		run(t, adminEnv, cli, "", "acl", "grant", "agent", capability, "read,invoke")
	}
	search := run(t, agentEnv, cli, "", "search", "API", "--format", "json")
	if !bytes.Contains(search, []byte("httpapi/http-api/get-secret")) || !bytes.Contains(search, []byte("httpapi/http-api/echo")) {
		t.Fatalf("search=%s", search)
	}
	skills := filepath.Join(root, "skills")
	run(t, agentEnv, cli, "", "project", "add", "httpapi/http-api/get-secret", skills)
	projected, err := os.ReadFile(filepath.Join(skills, "ninea-httpapi-http-api-get-secret", "SKILL.md"))
	if err != nil || !bytes.Contains(projected, []byte("Returns private API data")) {
		t.Fatalf("projected=%s error=%v", projected, err)
	}
	private := run(t, agentEnv, cli, `{"id":"123"}`, "invoke", "httpapi/http-api/get-secret")
	if !bytes.Contains(private, []byte(`"visibility":"private"`)) || !bytes.Contains(private, []byte(`"id":"123"`)) {
		t.Fatalf("private invoke=%s", private)
	}
	public := run(t, agentEnv, cli, `{"message":"hello"}`, "invoke", "httpapi/http-api/echo")
	if !bytes.Contains(public, []byte(`"visibility":"public"`)) || !bytes.Contains(public, []byte(`"echo":"hello"`)) {
		t.Fatalf("public invoke=%s", public)
	}
	callID := strings.TrimSpace(string(run(t, agentEnv, cli, `{"id":"async"}`, "calls", "start", "httpapi/http-api/get-secret")))
	completed := waitCall(t, agentEnv, cli, callID, "completed")
	if !bytes.Contains(completed.Result, []byte(`"id":"async"`)) {
		t.Fatalf("async=%#v", completed)
	}
	events := run(t, agentEnv, cli, "", "calls", "events", callID, "--limit", "10")
	if !bytes.Contains(events, []byte(`"type":"result"`)) {
		t.Fatalf("events=%s", events)
	}

	if err := d.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := d.Wait(); err != nil {
		t.Fatalf("graceful daemon stop: %v logs=%s", err, logs.String())
	}
	_ = os.Remove(socket)
	d = startDaemon(false)
	restored := run(t, agentEnv, cli, `{"message":"restored"}`, "invoke", "httpapi/http-api/echo")
	if !bytes.Contains(restored, []byte(`"echo":"restored"`)) {
		t.Fatalf("restored invoke=%s logs=%s", restored, logs.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if privateCalls != 2 || publicCalls != 2 {
		t.Fatalf("private calls=%d public calls=%d", privateCalls, publicCalls)
	}
}
