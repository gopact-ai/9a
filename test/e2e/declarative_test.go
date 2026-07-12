package e2e

import (
	"bytes"
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

func TestDeclarativeSkillCLIProjectionWorkflowAndRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("process e2e")
	}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Client") != "e2e-client" {
			t.Errorf("X-Client=%q", r.Header.Get("X-Client"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/location":
			_, _ = fmt.Fprintf(w, `{"lat":31.2,"city":%q}`, r.URL.Query().Get("city"))
		case "/weather":
			_, _ = w.Write([]byte(`{"temperature":26}`))
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
	cli, daemon := filepath.Join(bin, "9a"), filepath.Join(bin, "ninead")
	build(t, cli, "./cmd/9a")
	build(t, daemon, "./cmd/ninead")
	workspace := filepath.Join(root, "workspace")
	if err := os.Mkdir(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(workspace, "city-guide.yaml")
	source := fmt.Sprintf(`apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: city-guide
  description: Look up a city and its weather.
variables:
  client:
    fromEnv: CITY_CLIENT
    required: true
services:
  local:
    baseURL: %s
    headers:
      X-Client: "{{ vars.client }}"
operations:
  find-location:
    service: local
    method: GET
    path: /location
    request:
      query:
        city: "{{ input.city }}"
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body
  current-weather:
    service: local
    method: GET
    path: /weather
    hooks:
      afterResponse:
        - transform:
            language: jq
            expression: .body
workflows:
  city-report:
    steps:
      - id: location
        use: find-location
        input:
          city: "{{ input.city }}"
      - id: weather
        use: current-weather
    output:
      language: jq
      expression: '{city: .steps.location.city, temperature: .steps.weather.temperature}'
`, api.URL)
	if err := os.WriteFile(sourcePath, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	socket := socketPath(t)
	token := "declarative-e2e-admin"
	env := append(os.Environ(), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+token, "CITY_CLIENT=e2e-client", "PATH="+bin+":"+os.Getenv("PATH"))
	if output := runInDir(t, workspace, env, cli, "", "validate", sourcePath); !bytes.Contains(output, []byte(`"valid":true`)) {
		t.Fatalf("validate=%s", output)
	}
	state := filepath.Join(root, "state.db")
	var logs bytes.Buffer
	start := func(bootstrap bool) *exec.Cmd {
		command := exec.Command(daemon, "--state", state, "--socket", socket)
		command.Env = env
		if bootstrap {
			command.Env = append(command.Env, "NINEA_BOOTSTRAP_TOKEN="+token)
		}
		command.Stderr = &logs
		if err := command.Start(); err != nil {
			t.Fatal(err)
		}
		waitSocket(t, socket, &logs)
		return command
	}
	d := start(true)
	t.Cleanup(func() { _ = d.Process.Kill(); _ = d.Wait() })
	added := runInDir(t, workspace, env, cli, "", "add", sourcePath)
	if !bytes.Contains(added, []byte(`"name":"city-guide"`)) {
		t.Fatalf("add=%s", added)
	}
	skill := filepath.Join(workspace, ".agents", "skills", "city-guide")
	for _, path := range []string{"SKILL.md", "operations/find-location/invoke", "workflows/city-report/invoke", "references/source.yaml"} {
		if _, err := os.Stat(filepath.Join(skill, path)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	result := run(t, env, filepath.Join(skill, "workflows", "city-report", "invoke"), `{"city":"Shanghai"}`)
	if strings.TrimSpace(string(result)) != `{"city":"Shanghai","temperature":26}` {
		t.Fatalf("workflow=%s", result)
	}
	diff := runInDir(t, workspace, env, cli, "", "diff", sourcePath)
	if !bytes.Contains(diff, []byte(`"changed":false`)) {
		t.Fatalf("diff=%s", diff)
	}
	if err := d.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := d.Wait(); err != nil {
		t.Fatalf("stop: %v logs=%s", err, logs.String())
	}
	_ = os.Remove(socket)
	d = start(false)
	result = run(t, env, filepath.Join(skill, "operations", "find-location", "invoke"), `{"city":"Beijing"}`)
	if !bytes.Contains(result, []byte(`"city":"Beijing"`)) {
		t.Fatalf("restored=%s", result)
	}
	runInDir(t, workspace, env, cli, "", "remove", "city-guide")
	if _, err := os.Stat(skill); !os.IsNotExist(err) {
		t.Fatalf("skill remains: %v", err)
	}
}

func runInDir(t *testing.T, dir string, env []string, bin, input string, args ...string) []byte {
	t.Helper()
	command := exec.Command(bin, args...)
	command.Dir = dir
	command.Env = env
	command.Stdin = strings.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", bin, args, err, output)
	}
	return output
}
