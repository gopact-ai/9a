package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/api"
	callmodel "github.com/gopact-ai/9a/internal/call"
	"github.com/spf13/cobra"
)

func TestLocalPathsForHome(t *testing.T) {
	t.Parallel()
	home := filepath.Join(string(filepath.Separator), "home", "alice")
	dir := filepath.Join(home, ".local", "state", "ninea")
	got := localPathsForHome(home)
	want := localPaths{
		dir:    dir,
		state:  filepath.Join(dir, "ninea.db"),
		socket: filepath.Join(dir, "ninea.sock"),
		token:  filepath.Join(dir, "admin-token"),
		log:    filepath.Join(dir, "daemon.log"),
		pid:    filepath.Join(dir, "daemon.pid"),
		lock:   filepath.Join(dir, "daemon.lock"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("localPathsForHome(%q) = %#v, want %#v", home, got, want)
	}
}

func TestDaemonPathsFollowCustomStateAndSocket(t *testing.T) {
	t.Parallel()
	base := localPathsForHome(filepath.Join(string(filepath.Separator), "home", "alice"))
	state := filepath.Join(t.TempDir(), "test.db")
	socket := filepath.Join(t.TempDir(), "test.sock")
	got := daemonPaths(base, state, socket)
	want := localPaths{
		state:  state,
		socket: socket,
		token:  state + ".admin-token",
		log:    state + ".log",
		pid:    socket + ".pid",
		lock:   socket + ".lock",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("daemonPaths() = %#v, want %#v", got, want)
	}
}

func TestPrepareLocalPathsCreatesPrivateCustomDirectory(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "custom-state")
	options := daemonOptions{
		paths:  localPaths{},
		state:  filepath.Join(parent, "ninea.db"),
		socket: filepath.Join(parent, "ninea.sock"),
	}
	if err := prepareLocalPaths(options); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(parent)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("custom state directory mode=%o want 700", got)
	}
}

func TestPrepareLocalPathsRejectsInsecureCustomDirectory(t *testing.T) {
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	options := daemonOptions{
		paths:  localPaths{},
		state:  filepath.Join(parent, "ninea.db"),
		socket: filepath.Join(parent, "ninea.sock"),
	}
	err := prepareLocalPaths(options)
	if err == nil || !strings.Contains(err.Error(), "chmod 700") || !strings.Contains(err.Error(), parent) {
		t.Fatalf("prepareLocalPaths error=%v", err)
	}
	info, statErr := os.Stat(parent)
	if statErr != nil {
		t.Fatal(statErr)
	}
	if got := info.Mode().Perm(); got != 0o755 {
		t.Fatalf("custom directory was silently changed to %o", got)
	}
}

func TestStartupLockHelperProcess(t *testing.T) {
	if os.Getenv("NINEA_TEST_INHERITED_LOCK_HELPER") == "1" {
		file := os.NewFile(3, "inherited-daemon-lock")
		if file == nil {
			os.Exit(2)
		}
		_, _ = fmt.Fprintln(os.Stdout, "locked")
		time.Sleep(500 * time.Millisecond)
		_ = file.Close()
		os.Exit(0)
	}
	if os.Getenv("NINEA_TEST_LOCK_HELPER") != "1" {
		return
	}
	file, err := os.OpenFile(os.Getenv("NINEA_TEST_LOCK_PATH"), os.O_RDWR|os.O_CREATE, 0600)
	if err != nil || syscall.Flock(int(file.Fd()), syscall.LOCK_EX) != nil {
		os.Exit(2)
	}
	_, _ = fmt.Fprintln(os.Stdout, "locked")
	time.Sleep(30 * time.Second)
	os.Exit(0)
}

func TestAcquireStartupLockHasDeadline(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "daemon.lock")
	helper := exec.Command(os.Args[0], "-test.run=^TestStartupLockHelperProcess$")
	helper.Env = append(os.Environ(), "NINEA_TEST_LOCK_HELPER=1", "NINEA_TEST_LOCK_PATH="+lock)
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	helper.Stderr = os.Stderr
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	})
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("wait for lock helper: %v", err)
	}
	started := time.Now()
	lockFile, ready, err := acquireStartupLock(lock, filepath.Join(t.TempDir(), "missing.sock"), time.Now().Add(150*time.Millisecond))
	if lockFile != nil {
		_ = lockFile.Close()
	}
	if err == nil || ready {
		t.Fatalf("acquireStartupLock() = lock %v, ready %v, error %v", lockFile != nil, ready, err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("startup lock ignored deadline: %s", elapsed)
	}
}

func TestInheritedStartupLockSurvivesParentClose(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "daemon.lock")
	socket := filepath.Join(t.TempDir(), "missing.sock")
	lockFile, ready, err := acquireStartupLock(lock, socket, time.Now().Add(time.Second))
	if err != nil || ready {
		t.Fatalf("initial startup lock = ready %v, error %v", ready, err)
	}
	helper := exec.Command(os.Args[0], "-test.run=^TestStartupLockHelperProcess$")
	helper.Env = append(os.Environ(), "NINEA_TEST_INHERITED_LOCK_HELPER=1")
	helper.ExtraFiles = []*os.File{lockFile}
	stdout, err := helper.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := helper.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = helper.Process.Kill()
		_ = helper.Wait()
	})
	if _, err := bufio.NewReader(stdout).ReadString('\n'); err != nil {
		t.Fatalf("wait for inherited lock helper: %v", err)
	}
	if err := lockFile.Close(); err != nil {
		t.Fatalf("close parent startup lock: %v", err)
	}

	contender, contenderReady, err := acquireStartupLock(lock, socket, time.Now().Add(150*time.Millisecond))
	if contender != nil {
		_ = contender.Close()
	}
	if err == nil || contenderReady {
		t.Fatalf("inherited startup lock was lost: lock %v, ready %v, error %v", contender != nil, contenderReady, err)
	}
	if err := helper.Wait(); err != nil {
		t.Fatalf("inherited lock helper: %v", err)
	}
	acquired, acquiredReady, err := acquireStartupLock(lock, socket, time.Now().Add(time.Second))
	if err != nil || acquiredReady {
		t.Fatalf("startup lock after helper exit = ready %v, error %v", acquiredReady, err)
	}
	_ = acquired.Close()
}

func TestLoadOrCreateTokenCreatesPrivateTokenAndReusesIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "admin-token")
	first, err := loadOrCreateToken(path, "")
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(first, "ninea_") || info.Mode().Perm() != 0600 {
		t.Fatalf("token = %q, mode = %o", first, info.Mode().Perm())
	}
	second, err := loadOrCreateToken(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("second token = %q, want %q", second, first)
	}
	explicitPath := filepath.Join(t.TempDir(), "admin-token")
	explicit, err := loadOrCreateToken(explicitPath, "operator-token")
	if err != nil || explicit != "operator-token" {
		t.Fatalf("explicit token = %q, error = %v", explicit, err)
	}
	data, err := os.ReadFile(explicitPath)
	if err != nil || strings.TrimSpace(string(data)) != explicit {
		t.Fatalf("saved explicit token = %q, error = %v", data, err)
	}
	if _, err := loadOrCreateToken(filepath.Join(t.TempDir(), "admin-token"), " operator-token "); err == nil {
		t.Fatal("bootstrap token with surrounding whitespace was accepted")
	}
}

func TestDaemonCommandHelpDocumentsFlags(t *testing.T) {
	t.Parallel()
	cmd := newRootCommand(&cli{cwd: t.TempDir()})
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetErr(&output)
	cmd.SetArgs([]string{"daemon", "--help"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"9a daemon", "--state", "SQLite state file", "--socket", "Unix socket"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("daemon help is missing %q:\n%s", want, output.String())
		}
	}
}

func TestShutdownClosesHTTPThenAppThenDatabaseAndJoinsErrors(t *testing.T) {
	t.Parallel()
	serverErr := errors.New("server close failed")
	appErr := errors.New("app close failed")
	var order []string
	err := shutdown(
		context.Background(),
		func(context.Context) error { order = append(order, "http"); return serverErr },
		func(context.Context) error { order = append(order, "app"); return appErr },
		func() error { order = append(order, "db"); return nil },
	)
	if !reflect.DeepEqual(order, []string{"http", "app", "db"}) {
		t.Fatalf("shutdown order = %v", order)
	}
	if !errors.Is(err, serverErr) || !errors.Is(err, appErr) {
		t.Fatalf("shutdown error = %v", err)
	}
}

func TestLocalRPCConfigUsesEnvironmentThenLocalDefaults(t *testing.T) {
	t.Parallel()
	paths := localPathsForHome(t.TempDir())
	if err := os.MkdirAll(paths.dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.token, []byte(" local-token \n"), 0600); err != nil {
		t.Fatal(err)
	}
	socket, token, err := localRPCConfig(func(string) string { return "" }, paths)
	if err != nil || socket != paths.socket || token != "local-token" {
		t.Fatalf("default config = %q, %q, %v", socket, token, err)
	}
	env := map[string]string{"NINEA_SOCKET": "/tmp/operator.sock", "NINEA_TOKEN": "operator-token"}
	socket, token, err = localRPCConfig(func(key string) string { return env[key] }, paths)
	if err != nil || socket != env["NINEA_SOCKET"] || token != env["NINEA_TOKEN"] {
		t.Fatalf("environment config = %q, %q, %v", socket, token, err)
	}
}

func writeTestManifest(t *testing.T, dir string) (string, string) {
	t.Helper()
	source := "version: 1\n" +
		"name: weather\n" +
		"description: Current weather.\n" +
		"type: http\n" +
		"services:\n" +
		"  forecast:\n" +
		"    baseURL: https://example.com\n" +
		"capabilities:\n" +
		"  current:\n" +
		"    service: forecast\n" +
		"    method: GET\n" +
		"    path: /weather\n" +
		"    inputSchema: {}\n" +
		"    outputSchema: {}\n"
	file := filepath.Join(dir, "weather.yaml")
	if err := os.WriteFile(file, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	return file, source
}

func TestConnectRequestBuildsValidatedWorkspaceRequest(t *testing.T) {
	dir := t.TempDir()
	file, source := writeTestManifest(t, dir)
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := connectRequest(file, dir)
	if err != nil {
		t.Fatal(err)
	}
	want := api.Request{Action: "connect", Source: source, Root: canonical}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("connectRequest() = %#v, want %#v", got, want)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("kind: Skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := connectRequest(bad, dir); err == nil {
		t.Fatal("connectRequest accepted an invalid manifest")
	}
}

func TestConnectBootstrapsFreshWorkspaceWithoutDaemon(t *testing.T) {
	calls := 0
	c := &cli{
		cwd: t.TempDir(),
		call: func(api.Request) (json.RawMessage, error) {
			calls++
			return nil, errors.New("unexpected daemon call")
		},
	}
	routes, err := executeTestCommand(t, c, []string{"connect"}, "")
	if err != nil || !strings.Contains(routes, "9a connect --guide http --json") || calls != 0 {
		t.Fatalf("connect routes=%q calls=%d error=%v", routes, calls, err)
	}
	raw, err := executeTestCommand(t, c, []string{"connect", "--guide", "http", "--json"}, "")
	if err != nil || calls != 0 {
		t.Fatalf("connect guide calls=%d error=%v\n%s", calls, err, raw)
	}
	var guide struct {
		Type            string `json:"type"`
		ManifestVersion int    `json:"manifestVersion"`
		Template        string `json:"template"`
		Guide           string `json:"guide"`
		NextAction      struct {
			Command string `json:"command"`
		} `json:"nextAction"`
	}
	if err := json.Unmarshal([]byte(raw), &guide); err != nil {
		t.Fatalf("decode connect guide: %v\n%s", err, raw)
	}
	if guide.Type != "http" || guide.ManifestVersion != 1 || !strings.Contains(guide.Template, "inputSchema:") || !strings.Contains(guide.Guide, "strict format") || guide.NextAction.Command != "9a connect <manifest.yaml>" {
		t.Fatalf("connect guide=%#v", guide)
	}
	if _, err := executeTestCommand(t, c, []string{"connect", "--guide", "grpc"}, ""); err == nil {
		t.Fatal("connect guide accepted unsupported type")
	}
	if _, err := executeTestCommand(t, c, []string{"connect", "weather.yaml", "--guide", "http"}, ""); err == nil {
		t.Fatal("connect guide accepted a manifest argument")
	}
}

func TestCapabilityRunRequestAcceptsOneInputSource(t *testing.T) {
	dir := t.TempDir()
	inputFile := filepath.Join(dir, "request.json")
	if err := os.WriteFile(inputFile, []byte("{\"from\":\"file\"}"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		flagInput string
		stdin     string
		want      json.RawMessage
	}{
		{name: "empty", want: json.RawMessage("{}")},
		{name: "flag", flagInput: "{\"from\":\"flag\"}", want: json.RawMessage("{\"from\":\"flag\"}")},
		{name: "file", flagInput: "@" + inputFile, want: json.RawMessage("{\"from\":\"file\"}")},
		{name: "stdin", stdin: "{\"from\":\"stdin\"}", want: json.RawMessage("{\"from\":\"stdin\"}")},
		{name: "explicit stdin", flagInput: "-", stdin: "{\"from\":\"stdin\"}", want: json.RawMessage("{\"from\":\"stdin\"}")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := capabilityRunRequest("weather/current", test.flagInput, strings.NewReader(test.stdin))
			if err != nil {
				t.Fatal(err)
			}
			want := api.Request{Action: "run", Capability: "weather/current", Input: test.want}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("capabilityRunRequest() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestCapabilityRunRequestRejectsAmbiguousOrInvalidInput(t *testing.T) {
	tests := []struct {
		name       string
		capability string
		flagInput  string
		stdin      string
		want       string
	}{
		{name: "two sources", capability: "weather/current", flagInput: "{}", stdin: "{}", want: "only one"},
		{name: "malformed json", capability: "weather/current", flagInput: "{\"missing\":", want: "valid JSON"},
		{name: "missing integration", capability: "current", want: "<integration>/<capability>"},
		{name: "internal id", capability: "api/weather/current", want: "<integration>/<capability>"},
		{name: "empty integration", capability: "/current", want: "<integration>/<capability>"},
		{name: "empty capability", capability: "weather/", want: "<integration>/<capability>"},
		{name: "noncanonical capability", capability: "Weather/Current!", want: "<integration>/<capability>"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := capabilityRunRequest(test.capability, test.flagInput, strings.NewReader(test.stdin))
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want text %q", err, test.want)
			}
		})
	}

	valid := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, callmodel.MaxPayloadBytes-2)...)
	valid = append(valid, '"')
	request, err := capabilityRunRequest("weather/current", string(valid), strings.NewReader(""))
	if err != nil || len(request.Input) != callmodel.MaxPayloadBytes {
		t.Fatalf("maximum input len = %d, error = %v", len(request.Input), err)
	}
	oversized := append(valid, 'x')
	if _, err := capabilityRunRequest("weather/current", string(oversized), strings.NewReader("")); !errors.Is(err, callmodel.ErrPayloadTooLarge) {
		t.Fatalf("oversized input error = %v", err)
	}
}

func TestStatusDoctorAndDisconnectRequests(t *testing.T) {
	dir := t.TempDir()
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	status, err := statusRequest(dir, t.TempDir(), "")
	if err != nil {
		t.Fatal(err)
	}
	if want := (api.Request{Action: "status", Root: canonical}); !reflect.DeepEqual(status, want) {
		t.Fatalf("statusRequest() = %#v, want %#v", status, want)
	}
	doctor, err := doctorRequest(dir, t.TempDir(), true)
	if err != nil {
		t.Fatal(err)
	}
	if want := (api.Request{Action: "doctor", Root: canonical, Fix: true}); !reflect.DeepEqual(doctor, want) {
		t.Fatalf("doctorRequest() = %#v, want %#v", doctor, want)
	}
	disconnect, err := disconnectRequest("weather", dir)
	if err != nil {
		t.Fatal(err)
	}
	if want := (api.Request{Action: "disconnect", Name: "weather", Root: canonical}); !reflect.DeepEqual(disconnect, want) {
		t.Fatalf("disconnectRequest() = %#v, want %#v", disconnect, want)
	}
	if _, err := disconnectRequest(" ", dir); err == nil {
		t.Fatal("disconnectRequest accepted an empty integration")
	}
}

func TestSecretRequestsReadValuesOnlyFromInput(t *testing.T) {
	set, err := secretSetRequest("weather.api-token", strings.NewReader("private-value\n"), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if want := (api.Request{Action: "secret.set", Name: "weather.api-token", Value: "private-value"}); !reflect.DeepEqual(set, want) {
		t.Fatalf("secretSetRequest()=%#v want %#v", set, want)
	}
	list, err := secretListRequest("weather")
	if err != nil || !reflect.DeepEqual(list, api.Request{Action: "secret.list", Name: "weather"}) {
		t.Fatalf("secretListRequest()=%#v err=%v", list, err)
	}
	unset, err := secretUnsetRequest("weather.api-token")
	if err != nil || !reflect.DeepEqual(unset, api.Request{Action: "secret.unset", Name: "weather.api-token"}) {
		t.Fatalf("secretUnsetRequest()=%#v err=%v", unset, err)
	}
	for _, invalid := range []string{"", "weather", "Weather.token", "weather.token.extra"} {
		if _, err := secretSetRequest(invalid, strings.NewReader("value"), io.Discard); err == nil {
			t.Fatalf("secretSetRequest accepted %q", invalid)
		}
	}
	if _, err := secretSetRequest("weather.token", strings.NewReader(""), io.Discard); err == nil {
		t.Fatal("secretSetRequest accepted an empty value")
	}
	oversized := strings.Repeat("x", maxSecretBytes+1)
	if _, err := secretSetRequest("weather.token", strings.NewReader(oversized), io.Discard); err == nil {
		t.Fatal("secretSetRequest accepted an oversized value")
	}
}

func executeTestCommand(t *testing.T, c *cli, args []string, stdin string) (string, error) {
	t.Helper()
	root := newRootCommand(c)
	var stdout, stderr bytes.Buffer
	root.SetArgs(args)
	root.SetIn(strings.NewReader(stdin))
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	_, err := root.ExecuteC()
	return stdout.String(), err
}

func TestCLIExposesOnlyMainCommands(t *testing.T) {
	root := newRootCommand(&cli{cwd: t.TempDir()})
	got := map[string]bool{}
	for _, cmd := range root.Commands() {
		if !cmd.Hidden {
			got[cmd.Name()] = true
			if len(cmd.Aliases) != 0 {
				t.Fatalf("%s has aliases %v", cmd.Name(), cmd.Aliases)
			}
		}
	}
	want := []string{"connect", "disconnect", "doctor", "run", "search", "secret", "status"}
	if len(got) != len(want) {
		t.Fatalf("visible commands = %v, want %v", got, want)
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("visible commands %v missing %q", got, name)
		}
	}
	for _, old := range []string{"add", "validate", "diff", "remove", "invoke", "attach", "detach", "update", "providers", "adapters", "project", "calls", "acl", "tokens"} {
		if got[old] {
			t.Fatalf("legacy command %q is still visible", old)
		}
	}
}

func TestCLIValidatesBeforeCallingDaemon(t *testing.T) {
	dir := t.TempDir()
	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("kind: Skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  string
	}{
		{name: "invalid manifest", args: []string{"connect", bad}},
		{name: "invalid short reference", args: []string{"run", "api/weather/current"}, want: "<integration>/<capability>"},
		{name: "malformed input", args: []string{"run", "weather/current", "--input", "{\"missing\":"}, want: "valid JSON"},
		{name: "ambiguous input", args: []string{"run", "weather/current", "--input", "{}"}, stdin: "{}", want: "only one"},
		{name: "approval token missing", args: []string{"run", "weather/current", "--approve"}, want: "argument"},
		{name: "missing search query", args: []string{"search"}, want: "search term"},
		{name: "removed command", args: []string{"add", bad}, want: "unknown command"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			c := &cli{
				cwd: dir,
				call: func(api.Request) (json.RawMessage, error) {
					calls++
					return nil, nil
				},
			}
			_, err := executeTestCommand(t, c, test.args, test.stdin)
			if err == nil || (test.want != "" && !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error = %v, want text %q", err, test.want)
			}
			if calls != 0 {
				t.Fatalf("daemon called %d times before validation completed", calls)
			}
		})
	}
}

func TestCLIMapsMainCommandsToOneRequest(t *testing.T) {
	dir := t.TempDir()
	manifest, source := writeTestManifest(t, dir)
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	statusResponse := json.RawMessage(`{"state":"ready","integrations":[{"name":"weather","state":"ready","capabilities":1}]}`)
	doctorResponse := json.RawMessage(`{"healthy":true,"fixed":1,"checks":[{"name":"runtime","state":"ok","message":"local runtime responded"},{"name":"workspace","state":"fixed","message":"gateway view is ready"}]}`)
	searchResponse := json.RawMessage("[{\"ref\":\"weather/forecast\",\"name\":\"Weather forecast\",\"description\":\"Forecast by city\",\"requiresApproval\":false}]")
	tests := []struct {
		name       string
		args       []string
		stdin      string
		response   json.RawMessage
		want       api.Request
		wantStdout string
	}{
		{
			name:       "connect",
			args:       []string{"connect", manifest},
			response:   json.RawMessage("{\"name\":\"weather\",\"digest\":\"hidden\",\"root\":\"/hidden\",\"capabilities\":[\"weather/current\"]}"),
			want:       api.Request{Action: "connect", Source: source, Root: canonical},
			wantStdout: "Connected weather (1 capability)\n\nNext:\n  9a search weather --json\n\nSource:\n  .9a/integrations/weather.yaml\n",
		},
		{
			name:       "search",
			args:       []string{"search", "weather", "forecast"},
			response:   searchResponse,
			want:       api.Request{Action: "search", Query: "weather forecast", Root: canonical},
			wantStdout: "Capabilities (1)\n  weather/forecast\n    Name: Weather forecast\n    Description: Forecast by city\n    Inspect: 9a search weather/forecast --json\n",
		},
		{
			name:       "run flag",
			args:       []string{"run", "weather/current", "--input", "{\"city\":\"Shanghai\"}", "--approve", "v1.7.0123456789abcdef0123456789abcdef"},
			response:   json.RawMessage("{\"temperature\":21}"),
			want:       api.Request{Action: "run", Capability: "weather/current", Root: canonical, Input: json.RawMessage("{\"city\":\"Shanghai\"}"), Approval: "v1.7.0123456789abcdef0123456789abcdef"},
			wantStdout: "Result:\n  {\n    \"temperature\": 21\n  }\n",
		},
		{
			name:       "run stdin",
			args:       []string{"run", "weather/current"},
			stdin:      "{\"city\":\"Shanghai\"}",
			response:   json.RawMessage("null"),
			want:       api.Request{Action: "run", Capability: "weather/current", Root: canonical, Input: json.RawMessage("{\"city\":\"Shanghai\"}")},
			wantStdout: "Result:\n  null\n",
		},
		{
			name:       "status",
			args:       []string{"status"},
			response:   statusResponse,
			want:       api.Request{Action: "status", Root: canonical},
			wantStdout: "Ready\n  weather: ready (1 capability)\n",
		},
		{
			name:       "doctor fix",
			args:       []string{"doctor", "--fix"},
			response:   doctorResponse,
			want:       api.Request{Action: "doctor", Root: canonical, Fix: true},
			wantStdout: "Healthy (1 fixed)\n  runtime: ok — local runtime responded\n  workspace: fixed — gateway view is ready\n",
		},
		{
			name:       "disconnect",
			args:       []string{"disconnect", "weather"},
			response:   json.RawMessage("null"),
			want:       api.Request{Action: "disconnect", Name: "weather", Root: canonical},
			wantStdout: "Disconnected weather\n",
		},
		{
			name:       "secret set",
			args:       []string{"secret", "set", "weather.api-token"},
			stdin:      "private-value\n",
			response:   json.RawMessage("null"),
			want:       api.Request{Action: "secret.set", Name: "weather.api-token", Root: canonical, Value: "private-value"},
			wantStdout: "Stored weather.api-token\n",
		},
		{
			name:       "secret list",
			args:       []string{"secret", "list", "weather"},
			response:   json.RawMessage(`[{"name":"weather.api-token","state":"present"}]`),
			want:       api.Request{Action: "secret.list", Name: "weather", Root: canonical},
			wantStdout: "Secrets\n  weather.api-token: present\n",
		},
		{
			name:       "secret unset",
			args:       []string{"secret", "unset", "weather.api-token"},
			response:   json.RawMessage("null"),
			want:       api.Request{Action: "secret.unset", Name: "weather.api-token", Root: canonical},
			wantStdout: "Removed weather.api-token\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests []api.Request
			c := &cli{
				cwd: dir,
				call: func(request api.Request) (json.RawMessage, error) {
					requests = append(requests, request)
					return test.response, nil
				},
			}
			stdout, err := executeTestCommand(t, c, test.args, test.stdin)
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != 1 || !reflect.DeepEqual(requests[0], test.want) {
				t.Fatalf("requests = %#v, want [%#v]", requests, test.want)
			}
			if stdout != test.wantStdout {
				t.Fatalf("stdout = %q, want %q", stdout, test.wantStdout)
			}
		})
	}
}

func TestHumanSearchExplainsEmptyAndApprovalStates(t *testing.T) {
	empty, err := humanSearch(json.RawMessage(`[]`))
	if err != nil {
		t.Fatal(err)
	}
	if empty != "No capabilities found.\nNext: try a broader query or run 9a status.\n" {
		t.Fatalf("empty search output = %q", empty)
	}

	approval, err := humanSearch(json.RawMessage(`[{
		"ref":"orders/create",
		"name":"Create order",
		"description":"Create one order.",
		"requiresApproval":true
	}]`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(approval, "Approval: required before execution") {
		t.Fatalf("approval search output = %q", approval)
	}
}

func TestConnectOutputUsesResponseSourceAndIntegrationSearch(t *testing.T) {
	output, err := humanResponse(
		api.Request{Action: "connect"},
		json.RawMessage("{\"name\":\"weather\",\"source\":\".9a/integrations/custom.yaml\",\"capabilities\":[\"mcp/weather/zulu\",\"api/weather/alpha\"]}"),
	)
	if err != nil {
		t.Fatal(err)
	}
	want := "Connected weather (2 capabilities)\n\nNext:\n  9a search weather --json\n\nSource:\n  .9a/integrations/custom.yaml\n"
	if output != want {
		t.Fatalf("output = %q, want %q", output, want)
	}
}

func TestHumanStatusExplainsReadiness(t *testing.T) {
	tests := []struct {
		name     string
		response string
		want     string
	}{
		{name: "empty", response: `{"state":"empty","integrations":[]}`, want: "Not ready\n  No integrations connected.\n  Next: 9a connect <manifest.yaml>\n"},
		{name: "ready", response: `{"state":"ready","integrations":[{"name":"weather","state":"ready","capabilities":1}]}`, want: "Ready\n  weather: ready (1 capability)\n"},
		{name: "needs secret", response: `{"state":"needs-secret","integrations":[{"name":"weather","state":"needs-secret","capabilities":2,"missingSecrets":["weather.api-token"]}]}`, want: "Needs secret\n  weather: needs-secret (2 capabilities)\n    Missing: weather.api-token\n    Next: 9a secret set weather.api-token\n"},
		{name: "broken", response: `{"state":"broken","integrations":[{"name":"weather","state":"broken","capabilities":1,"message":"source and runtime differ"}]}`, want: "Broken\n  weather: broken (1 capability) — source and runtime differ\n    Next: 9a doctor\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := humanResponse(api.Request{Action: "status"}, json.RawMessage(test.response))
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("status = %q, want %q", got, test.want)
			}
		})
	}
}

func TestHumanOutputEscapesTerminalControls(t *testing.T) {
	tests := []struct {
		name     string
		request  api.Request
		response json.RawMessage
		want     []string
	}{
		{
			name:     "connect",
			request:  api.Request{Action: "connect"},
			response: json.RawMessage("{\"name\":\"evil\\u001b]0;owned\\u0007\",\"source\":\".9a/integrations/evil.yaml\",\"capabilities\":[\"evil/run\"]}"),
			want:     []string{"evil\\u001b]0;owned\\u0007", "9a search evil\\u001b]0;owned\\u0007 --json"},
		},
		{
			name:     "search",
			request:  api.Request{Action: "search"},
			response: json.RawMessage("[{\"ref\":\"evil/run\",\"name\":\"Run\\u001b]2;owned\\u0007\\nforged\",\"description\":\"red\\u001b[31m text\"}]"),
			want:     []string{"Name: Run\\u001b]2;owned\\u0007 forged", "Description: red\\u001b[31m text", "9a search evil/run --json"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			output, err := humanResponse(test.request, test.response)
			if err != nil {
				t.Fatal(err)
			}
			if strings.ContainsAny(output, "\x1b\a\r\t") || strings.Contains(output, "\nforged") {
				t.Fatalf("unsafe terminal control in output %q", output)
			}
			for _, want := range test.want {
				if !strings.Contains(output, want) {
					t.Fatalf("output missing %q: %q", want, output)
				}
			}
		})
	}
}

func TestJSONFlagReturnsMachineReadableOutput(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name     string
		args     []string
		response json.RawMessage
		want     string
	}{
		{name: "search", args: []string{"search", "weather", "--json"}, response: json.RawMessage("[]"), want: "[]\n"},
		{name: "run null", args: []string{"run", "weather/current", "--json"}, response: json.RawMessage("null"), want: "null\n"},
		{name: "disconnect", args: []string{"disconnect", "weather", "--json"}, response: json.RawMessage("null"), want: "{\"ok\":true}\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := &cli{
				cwd: dir,
				call: func(api.Request) (json.RawMessage, error) {
					return test.response, nil
				},
			}
			stdout, err := executeTestCommand(t, c, test.args, "")
			if err != nil {
				t.Fatal(err)
			}
			if stdout != test.want || !json.Valid([]byte(strings.TrimSpace(stdout))) {
				t.Fatalf("stdout = %q, want valid JSON %q", stdout, test.want)
			}
		})
	}
}

func TestRemoteErrorStillFormatsPartialRunResult(t *testing.T) {
	remote := &rpcError{
		code:    "run_failed",
		message: "capability failed",
		data:    json.RawMessage("{\"call_id\":\"call-1\"}"),
	}
	c := &cli{
		cwd: t.TempDir(),
		call: func(api.Request) (json.RawMessage, error) {
			return nil, remote
		},
	}
	stdout, err := executeTestCommand(t, c, []string{"run", "weather/current"}, "")
	if err == nil || !strings.Contains(err.Error(), "run_failed") {
		t.Fatalf("error = %v", err)
	}
	if want := "Call: call-1\n"; stdout != want {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
	if len(remote.data) != 0 {
		t.Fatalf("partial data was left for raw stderr output: %s", remote.data)
	}
}

func TestApprovalErrorShowsSafeNextActionInsteadOfResult(t *testing.T) {
	remote := &rpcError{
		code:    "approval_required",
		message: "approval required to run weather/update",
		data:    json.RawMessage(`{"approvalToken":"v1.7.0123456789abcdef0123456789abcdef","sideEffect":"none","nextAction":{"instruction":"Obtain explicit approval, then retry the exact same input with --approve v1.7.0123456789abcdef0123456789abcdef"}}`),
	}
	c := &cli{
		cwd: t.TempDir(),
		call: func(api.Request) (json.RawMessage, error) {
			return nil, remote
		},
	}
	stdout, err := executeTestCommand(t, c, []string{"run", "weather/update"}, "")
	if err == nil || !strings.Contains(err.Error(), "approval_required") {
		t.Fatalf("error = %v", err)
	}
	want := "Next:\n  Obtain explicit approval, then retry the exact same input with --approve v1.7.0123456789abcdef0123456789abcdef\nNothing was sent upstream.\n"
	if stdout != want || strings.Contains(stdout, "Result:") {
		t.Fatalf("stdout = %q, want %q", stdout, want)
	}
}

func TestJSONErrorPreservesTheActionableEnvelope(t *testing.T) {
	remote := &rpcError{
		code:    "approval_required",
		message: "approval required to run weather/update",
		data:    json.RawMessage(`{"approvalToken":"v1.7.0123456789abcdef0123456789abcdef","sideEffect":"none","nextAction":{"instruction":"Obtain explicit approval, then retry the exact same input with --approve v1.7.0123456789abcdef0123456789abcdef"}}`),
	}
	c := &cli{
		cwd: t.TempDir(),
		call: func(api.Request) (json.RawMessage, error) {
			return nil, remote
		},
	}
	stdout, err := executeTestCommand(t, c, []string{"run", "weather/update", "--json"}, "")
	if err == nil {
		t.Fatal("JSON run error was reported as success")
	}
	var envelope struct {
		Code  string `json:"code"`
		Error string `json:"error"`
		Data  struct {
			ApprovalToken string `json:"approvalToken"`
			SideEffect    string `json:"sideEffect"`
			NextAction    struct {
				Instruction string `json:"instruction"`
			} `json:"nextAction"`
		} `json:"data"`
	}
	if decodeErr := json.Unmarshal([]byte(stdout), &envelope); decodeErr != nil {
		t.Fatalf("decode JSON error: %v\n%s", decodeErr, stdout)
	}
	if envelope.Code != "approval_required" || envelope.Data.ApprovalToken != "v1.7.0123456789abcdef0123456789abcdef" || envelope.Data.SideEffect != "none" || envelope.Data.NextAction.Instruction != "Obtain explicit approval, then retry the exact same input with --approve v1.7.0123456789abcdef0123456789abcdef" {
		t.Fatalf("error envelope=%#v", envelope)
	}
	if len(remote.data) != 0 {
		t.Fatalf("error data was left for duplicate stderr output: %s", remote.data)
	}
}

func TestTopLevelMachineErrorWrapsLocalValidation(t *testing.T) {
	var stdout bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	if err := writeTopLevelMachineError(cmd, errors.New("capability must use <integration>/<capability>")); err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Code  string `json:"code"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &envelope); err != nil {
		t.Fatalf("decode local error: %v\n%s", err, stdout.String())
	}
	if envelope.Code != "invalid_request" || !strings.Contains(envelope.Error, "integration") {
		t.Fatalf("local error envelope=%#v", envelope)
	}
}

func TestTopLevelMachineErrorMarksUnknownRunTransportOutcome(t *testing.T) {
	var stdout bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	err := &rpcTransportError{action: "run", requestMayHaveSent: true, err: io.ErrUnexpectedEOF}
	if writeErr := writeTopLevelMachineError(cmd, err); writeErr != nil {
		t.Fatal(writeErr)
	}
	var envelope struct {
		Code string `json:"code"`
		Data struct {
			SideEffect string `json:"sideEffect"`
			Retryable  bool   `json:"retryable"`
		} `json:"data"`
	}
	if decodeErr := json.Unmarshal(stdout.Bytes(), &envelope); decodeErr != nil {
		t.Fatalf("decode transport error: %v\n%s", decodeErr, stdout.String())
	}
	if envelope.Code != "transport_error" || envelope.Data.SideEffect != "possible" || envelope.Data.Retryable {
		t.Fatalf("transport envelope=%#v", envelope)
	}
}

func TestWriteCommandOutputRejectsInvalidDaemonJSON(t *testing.T) {
	var stdout bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&stdout)
	c := &cli{call: func(api.Request) (json.RawMessage, error) {
		return json.RawMessage("not-json"), nil
	}}
	if err := c.runRequest(cmd, api.Request{Action: "run"}); err == nil {
		t.Fatal("invalid daemon JSON was accepted")
	}
}

func TestDataCommandHelpDocumentsJSONAndRunInputs(t *testing.T) {
	c := &cli{cwd: t.TempDir()}
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"run", "--help"}, want: []string{"--json", "--input", "--approve", "@file", "stdin"}},
		{args: []string{"connect", "--help"}, want: []string{"9a connect [manifest.yaml]", "--guide", "--json"}},
		{args: []string{"search", "--help"}, want: []string{"9a search <query...>", "--json"}},
	} {
		output, err := executeTestCommand(t, c, test.args, "")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range test.want {
			if !strings.Contains(output, want) {
				t.Fatalf("9a %s help missing %q:\n%s", strings.Join(test.args, " "), want, output)
			}
		}
	}
}

func TestCompletionDoesNotCallDaemon(t *testing.T) {
	calls := 0
	c := &cli{
		cwd: t.TempDir(),
		call: func(api.Request) (json.RawMessage, error) {
			calls++
			return nil, nil
		},
	}
	stdout, err := executeTestCommand(t, c, []string{"completion", "bash"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !strings.Contains(stdout, "__start_9a") {
		t.Fatalf("calls = %d, completion = %q", calls, stdout)
	}
}

func TestVersionCommandAndFlagAgreeWithoutDaemon(t *testing.T) {
	newTestCLI := func() *cli {
		return &cli{
			cwd: t.TempDir(),
			call: func(api.Request) (json.RawMessage, error) {
				t.Fatal("version contacted daemon")
				return nil, nil
			},
		}
	}
	commandOutput, err := executeTestCommand(t, newTestCLI(), []string{"version"}, "")
	if err != nil {
		t.Fatal(err)
	}
	flagOutput, err := executeTestCommand(t, newTestCLI(), []string{"--version"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if commandOutput != flagOutput || !strings.HasPrefix(commandOutput, "9a ") {
		t.Fatalf("version command = %q, flag = %q", commandOutput, flagOutput)
	}
}

func TestVersionShortcutHonorsJSONFlagInEitherOrder(t *testing.T) {
	for _, args := range [][]string{{"--json", "--version"}, {"--version", "--json"}} {
		output, err := executeTestCommand(t, &cli{cwd: t.TempDir()}, args, "")
		if err != nil {
			t.Fatalf("9a %s: %v", strings.Join(args, " "), err)
		}
		if !json.Valid([]byte(strings.TrimSpace(output))) || !strings.Contains(output, "\"version\"") {
			t.Fatalf("9a %s output = %q", strings.Join(args, " "), output)
		}
	}
}

func TestHelpDocumentsMainPathWithoutCallingDaemon(t *testing.T) {
	tests := []struct {
		args []string
		want []string
	}{
		{want: []string{"Commands:", "connect", "search", "run", "status", "disconnect", "doctor", "secret", "9a <command> --help"}},
		{args: []string{"help", "connect"}, want: []string{"9a connect [manifest.yaml]", "--guide"}},
		{args: []string{"help", "run"}, want: []string{"9a run <integration>/<capability>", "--input"}},
		{args: []string{"help", "status"}, want: []string{"9a status", "--workspace"}},
		{args: []string{"help", "disconnect"}, want: []string{"9a disconnect <integration>"}},
		{args: []string{"help", "doctor"}, want: []string{"9a doctor", "--fix", "--workspace"}},
		{args: []string{"help", "secret"}, want: []string{"set", "list", "unset"}},
		{args: []string{"help", "secret", "set"}, want: []string{"9a secret set <integration>.<key>", "stdin"}},
	}
	for _, test := range tests {
		calls := 0
		c := &cli{
			cwd: t.TempDir(),
			call: func(api.Request) (json.RawMessage, error) {
				calls++
				return nil, nil
			},
		}
		output, err := executeTestCommand(t, c, test.args, "")
		if err != nil {
			t.Fatalf("9a %s: %v\n%s", strings.Join(test.args, " "), err, output)
		}
		for _, want := range test.want {
			if !strings.Contains(output, want) {
				t.Fatalf("9a %s help missing %q:\n%s", strings.Join(test.args, " "), want, output)
			}
		}
		if calls != 0 {
			t.Fatalf("9a %s help contacted daemon %d times", strings.Join(test.args, " "), calls)
		}
	}
}
