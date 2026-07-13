package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func build(t *testing.T, out, pkg string) {
	t.Helper()
	c := exec.Command("go", "build", "-o", out, pkg)
	c.Dir = "../.."
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
}
func socketPath(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "ninea-e2e-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func isolatedEnv(home string, overrides ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(overrides)+1)
	for _, value := range os.Environ() {
		if strings.HasPrefix(value, "NINEA_") || strings.HasPrefix(value, "HOME=") {
			continue
		}
		env = append(env, value)
	}
	return append(env, append([]string{"HOME=" + home}, overrides...)...)
}

func run(t *testing.T, env []string, bin string, input string, args ...string) []byte {
	t.Helper()
	if len(args) == 4 && args[0] == "project" && args[1] == "add" {
		cleanupReadOnlyTree(t, filepath.Dir(args[3]))
	}
	c := exec.Command(bin, args...)
	c.Env = append(env, "NINEA_AUTO_ATTACH=0")
	c.Stdin = strings.NewReader(input)
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, b)
	}
	return b
}

func cleanupReadOnlyTree(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				_ = os.Chmod(path, 0o700)
			}
			return nil
		})
	})
}
func runFails(t *testing.T, env []string, bin string, input string, args ...string) []byte {
	t.Helper()
	c := exec.Command(bin, args...)
	c.Env = append(env, "NINEA_AUTO_ATTACH=0")
	c.Stdin = strings.NewReader(input)
	b, err := c.CombinedOutput()
	if err == nil {
		t.Fatalf("%s %v unexpectedly passed: %s", bin, args, b)
	}
	return b
}

func TestMCPDiscoverySearchProjectionAndInvoke(t *testing.T) {
	if testing.Short() {
		t.Skip("process e2e")
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	_ = os.Mkdir(bin, 0755)
	cli, fixture := filepath.Join(bin, "9a"), filepath.Join(bin, "mcpfixture")
	build(t, cli, "./cmd/9a")
	build(t, fixture, "./testdata/mcpserver")
	socket := socketPath(t)
	token := "e2e-secret"
	counter := filepath.Join(root, "provider-calls")
	adminEnv := isolatedEnv(filepath.Join(root, "home"), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+token, "PATH="+bin+":"+os.Getenv("PATH"), "NINEA_FIXTURE_COUNTER="+counter)
	d := exec.Command(cli, "daemon", "--state", filepath.Join(root, "state.db"), "--socket", socket)
	d.Env = append(adminEnv, "NINEA_BOOTSTRAP_TOKEN="+token)
	var logs bytes.Buffer
	d.Stderr = &logs
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Process.Kill(); _ = d.Wait() })
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon socket: %s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	run(t, adminEnv, cli, "", "providers", "add", "mcp", "weather", "stdio:"+fixture)
	malformed := filepath.Join(root, "malformed-provider")
	if err := os.WriteFile(malformed, []byte("#!/bin/sh\nMCP_FIXTURE_MODE=malformed exec \""+fixture+"\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if out := runFails(t, adminEnv, cli, "", "providers", "add", "mcp", "weather", "stdio:"+malformed); !bytes.Contains(out, []byte("missing tools")) {
		t.Fatalf("malformed provider result=%s", out)
	}
	agentToken := strings.TrimSpace(string(run(t, adminEnv, cli, "", "tokens", "create", "agent")))
	agentEnv := isolatedEnv(filepath.Join(root, "home"), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+agentToken, "PATH="+bin+":"+os.Getenv("PATH"))
	if out := runFails(t, agentEnv, cli, "", "acl", "grant", "agent", "mcp/weather/get-weather", "invoke"); !bytes.Contains(out, []byte("permission_denied")) {
		t.Fatalf("self grant=%s", out)
	}
	search := run(t, agentEnv, cli, "", "search", "temperature", "--format", "json")
	var results []map[string]any
	if err := json.Unmarshal(search, &results); err != nil || len(results) != 0 {
		t.Fatalf("search before grant=%s err=%v", search, err)
	}
	run(t, adminEnv, cli, "", "acl", "grant", "agent", "mcp/weather/get-weather", "read")
	search = run(t, agentEnv, cli, "", "search", "temperature", "--format", "json")
	if err := json.Unmarshal(search, &results); err != nil || len(results) != 1 {
		t.Fatalf("search=%s err=%v", search, err)
	}
	skills := filepath.Join(root, "skills")
	run(t, agentEnv, cli, "", "project", "add", "mcp/weather/get-weather", skills)
	skill := filepath.Join(skills, "ninea-mcp-weather-get-weather")
	cat := exec.Command("cat", filepath.Join(skill, "SKILL.md"))
	b, err := cat.Output()
	if err != nil || !bytes.Contains(b, []byte("Get current weather")) {
		t.Fatalf("SKILL.md=%s err=%v", b, err)
	}
	if calls, err := os.ReadFile(counter); err == nil && len(calls) > 0 {
		t.Fatalf("passive read invoked provider: %s", calls)
	}
	run(t, adminEnv, cli, "", "acl", "grant", "agent", "mcp/weather/get-forecast", "read")
	forecast := run(t, agentEnv, cli, "", "search", "forecast", "--format", "json")
	var forecastResults []map[string]any
	if err := json.Unmarshal(forecast, &forecastResults); err != nil || len(forecastResults) != 1 {
		t.Fatalf("paginated search=%s err=%v", forecast, err)
	}
	denied := runFails(t, agentEnv, filepath.Join(skill, "scripts/invoke"), `{"location":"Shanghai"}`)
	if !bytes.Contains(denied, []byte("permission_denied")) {
		t.Fatalf("invoke denial=%s", denied)
	}
	run(t, adminEnv, cli, "", "acl", "grant", "agent", "mcp/weather/get-weather", "invoke")
	if err := d.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = d.Wait()
	_ = os.Remove(socket)
	d2 := exec.Command(cli, "daemon", "--state", filepath.Join(root, "state.db"), "--socket", socket)
	d2.Env = adminEnv
	d2.Stderr = &logs
	if err := d2.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d2.Process.Kill(); _ = d2.Wait() })
	for deadline := time.Now().Add(5 * time.Second); ; {
		if _, err := os.Stat(socket); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("restarted daemon socket: %s", logs.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	out := run(t, agentEnv, filepath.Join(skill, "scripts/invoke"), `{"location":"Shanghai"}`)
	var toolResult struct {
		Content []struct{ Type, Text string } `json:"content"`
		IsError bool                          `json:"isError"`
	}
	if err := json.Unmarshal(out, &toolResult); err != nil || toolResult.IsError || len(toolResult.Content) != 1 || !strings.Contains(toolResult.Content[0].Text, `"temperature":26`) {
		t.Fatalf("invoke=%s err=%v", out, err)
	}
	if calls, err := os.ReadFile(counter); err != nil || strings.Count(string(calls), "call\n") != 1 {
		t.Fatalf("provider calls=%q err=%v", calls, err)
	}
	run(t, adminEnv, cli, "", "providers", "remove", "mcp", "weather")
	if _, err := os.Stat(skill); !os.IsNotExist(err) {
		t.Fatalf("provider projection remains: %v", err)
	}
}

func TestDaemonRejectsMissingToken(t *testing.T) {
	root := t.TempDir()
	cli := filepath.Join(root, "9a")
	build(t, cli, "./cmd/9a")
	socket := socketPath(t)
	d := exec.Command(cli, "daemon", "--state", filepath.Join(root, "s.db"), "--socket", socket)
	d.Env = isolatedEnv(filepath.Join(root, "home"), "NINEA_BOOTSTRAP_TOKEN=right")
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Process.Kill(); _ = d.Wait() })
	for i := 0; i < 100; i++ {
		if _, e := os.Stat(socket); e == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	c := exec.Command(cli, "search", "weather")
	c.Env = isolatedEnv(filepath.Join(root, "home"), "NINEA_SOCKET="+socket, "NINEA_TOKEN=wrong")
	b, err := c.CombinedOutput()
	if err == nil || !bytes.Contains(b, []byte("unauthorized")) {
		t.Fatalf("wrong token result err=%v output=%s", err, b)
	}
}
