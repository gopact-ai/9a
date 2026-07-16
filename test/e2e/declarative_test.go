package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestConnectRunRestartAndDisconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("process e2e")
	}

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/current" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-Client"); got != "e2e-client" {
			t.Errorf("X-Client = %q, want e2e-client", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"city":%q,"temperature":26}`, r.URL.Query().Get("city"))
	}))
	defer api.Close()

	root := t.TempDir()
	binDir := filepath.Join(root, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(binDir, "9a")
	build(t, cli, "./cmd/9a")

	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	cleanupReadOnlyDirectories(t, workspace)
	manifest := fmt.Sprintf(`version: 1
name: city-guide
description: Read the current weather for a city.
type: http
services:
  local:
    baseURL: %s
    headers:
      X-Client: e2e-client
capabilities:
  current:
    description: Read current weather.
    service: local
    method: GET
    path: /current
    request:
      query:
        city: "{{ input.city }}"
    inputSchema:
      type: object
      required: [city]
      properties:
        city:
          type: string
    outputSchema:
      type: object
      required: [city, temperature]
      properties:
        city:
          type: string
        temperature:
          type: number
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body
`, api.URL)
	manifestPath := filepath.Join(workspace, "city-guide.yaml")
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}

	socket := socketPath(t)
	state := filepath.Join(filepath.Dir(socket), "state.db")
	token := "declarative-e2e-admin"
	env := isolatedEnv(
		filepath.Join(root, "home"),
		"NINEA_SOCKET="+socket,
		"NINEA_TOKEN="+token,
		"PATH="+binDir+":"+os.Getenv("PATH"),
	)
	var logs lockedBuffer
	startDaemon := func(bootstrap bool) *exec.Cmd {
		command := exec.Command(cli, "daemon", "--state", state, "--socket", socket)
		command.Env = env
		if bootstrap {
			command.Env = append(command.Env, "NINEA_BOOTSTRAP_TOKEN="+token)
		}
		command.Stderr = &logs
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = command.Process.Kill()
			_ = command.Wait()
		})
		waitSocket(t, socket, &logs)
		return command
	}
	stopDaemon := func(command *exec.Cmd) {
		if err := command.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		if err := command.Wait(); err != nil {
			t.Fatalf("stop daemon: %v\n%s", err, logs.String())
		}
		_ = os.Remove(socket)
	}

	daemon := startDaemon(true)
	connected := runInDir(t, workspace, env, cli, "", "connect", manifestPath, "--json")
	var connectResult struct {
		Name         string   `json:"name"`
		Source       string   `json:"source"`
		Capabilities []string `json:"capabilities"`
	}
	if err := json.Unmarshal(connected, &connectResult); err != nil {
		t.Fatalf("decode connect response: %v\n%s", err, connected)
	}
	if connectResult.Name != "city-guide" || connectResult.Source != ".9a/integrations/city-guide.yaml" || len(connectResult.Capabilities) != 1 || connectResult.Capabilities[0] != "city-guide/current" {
		t.Fatalf("connect response = %#v", connectResult)
	}

	canonical := filepath.Join(workspace, ".9a", "integrations", "city-guide.yaml")
	if source, err := os.ReadFile(canonical); err != nil || string(source) != manifest {
		t.Fatalf("canonical source = %q, error = %v", source, err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agents", "skills", "using-ninea", "SKILL.md")); err != nil {
		t.Fatalf("gateway projection: %v", err)
	}
	if _, err := os.Stat(filepath.Join(workspace, ".agents", "skills", "city-guide")); !os.IsNotExist(err) {
		t.Fatalf("integration should not be projected as a skill: %v", err)
	}

	search := runInDir(t, workspace, env, cli, "", "search", "current", "--json")
	var searchResults []struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(search, &searchResults); err != nil || len(searchResults) != 1 || searchResults[0].Ref != "city-guide/current" {
		t.Fatalf("search response = %s, error = %v", search, err)
	}
	if status := runInDir(t, workspace, env, cli, "", "status"); string(status) != "Ready\n  city-guide: ready (1 capability)\n" {
		t.Fatalf("status = %q, want Ready", status)
	}
	assertRunResult(t, runInDir(t, workspace, env, cli, "", "run", "city-guide/current", "--input", `{"city":"Shanghai"}`, "--json"), "Shanghai")

	mcpServer := filepath.Join(binDir, "mcp-server")
	mcpScript := `#!/bin/sh
while IFS= read -r line; do
  case "$line" in
    *'"method":"initialize"'*) printf '%s\n' '{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2025-11-25"}}' ;;
    *'"method":"tools/list"'*) printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"tools":[{"name":"echo","description":"Echo input","inputSchema":{"type":"object"},"annotations":{"readOnlyHint":true}}]}}' ;;
    *'"method":"tools/call"'*) printf '%s\n' '{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"mcp-ok"}]}}' ;;
  esac
done
`
	if err := os.WriteFile(mcpServer, []byte(mcpScript), 0o700); err != nil {
		t.Fatal(err)
	}
	mcpConnected := runInDir(t, workspace, env, cli, "", "connect", "mcp", "--name", "local-tools", "--json", "--", mcpServer)
	if err := json.Unmarshal(mcpConnected, &connectResult); err != nil || connectResult.Name != "local-tools" || len(connectResult.Capabilities) != 1 || connectResult.Capabilities[0] != "local-tools/echo" {
		t.Fatalf("MCP connect response = %s, error = %v", mcpConnected, err)
	}
	mcpResult := runApprovedInDir(t, workspace, env, cli, "", "run", "local-tools/echo", "--input", `{}`, "--json")
	if !bytes.Contains(mcpResult, []byte(`"text":"mcp-ok"`)) {
		t.Fatalf("MCP run response = %s", mcpResult)
	}

	if err := os.Remove(manifestPath); err != nil {
		t.Fatal(err)
	}
	stopDaemon(daemon)
	_ = startDaemon(false)
	assertRunResult(t, runInDir(t, workspace, env, cli, `{"city":"Beijing"}`, "run", "city-guide/current", "--input", "-", "--json"), "Beijing")
	mcpResult = runApprovedInDir(t, workspace, env, cli, "", "run", "local-tools/echo", "--input", `{}`, "--json")
	if !bytes.Contains(mcpResult, []byte(`"text":"mcp-ok"`)) {
		t.Fatalf("restored MCP run response = %s", mcpResult)
	}

	runInDir(t, workspace, env, cli, "", "disconnect", "city-guide")
	if source, err := os.ReadFile(canonical); err != nil || string(source) != manifest {
		t.Fatalf("source after disconnect = %q, error = %v", source, err)
	}
	search = runInDir(t, workspace, env, cli, "", "search", "current", "--json")
	if bytes.Contains(search, []byte("city-guide/current")) {
		t.Fatalf("disconnected capability remains searchable: %s", search)
	}
}

func assertRunResult(t *testing.T, output []byte, city string) {
	t.Helper()
	var result struct {
		City        string `json:"city"`
		Temperature int    `json:"temperature"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("decode run result: %v\n%s", err, output)
	}
	if result.City != city || result.Temperature != 26 {
		t.Fatalf("run result = %#v", result)
	}
}

func runInDir(t *testing.T, dir string, env []string, bin, input string, args ...string) []byte {
	t.Helper()
	output, err := runInDirResult(dir, env, bin, input, args...)
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, output)
	}
	return output
}

func runApprovedInDir(t *testing.T, dir string, env []string, bin, input string, args ...string) []byte {
	t.Helper()
	preflight, err := runInDirResult(dir, env, bin, input, args...)
	if err == nil {
		t.Fatalf("%s %v unexpectedly ran without approval\n%s", bin, args, preflight)
	}
	var failure struct {
		Code string `json:"code"`
		Data struct {
			ApprovalToken string `json:"approvalToken"`
			SideEffect    string `json:"sideEffect"`
		} `json:"data"`
	}
	if decodeErr := json.Unmarshal(preflight, &failure); decodeErr != nil || failure.Code != "approval_required" || failure.Data.ApprovalToken == "" || failure.Data.SideEffect != "none" {
		t.Fatalf("approval preflight=%s error=%v", preflight, decodeErr)
	}
	approved := append(append([]string(nil), args...), "--approve", failure.Data.ApprovalToken)
	return runInDir(t, dir, env, bin, input, approved...)
}

func runInDirResult(dir string, env []string, bin, input string, args ...string) ([]byte, error) {
	command := exec.Command(bin, args...)
	command.Dir = dir
	command.Env = env
	command.Stdin = strings.NewReader(input)
	return command.CombinedOutput()
}
