package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/9a/internal/api"
	callmodel "github.com/gopact-ai/9a/internal/call"
)

func TestAdapterAddRequest(t *testing.T) {
	got := adapterAddRequest("billing", "/opt/ninea/billing-adapter")
	want := api.Request{Action: "adapter.add", Protocol: "billing", Executable: "/opt/ninea/billing-adapter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("adapterAddRequest()=%#v want %#v", got, want)
	}
}

func TestDeclarativeCommandsBuildValidatedRequests(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "weather.yaml")
	source := `apiVersion: 9a.dev/v1alpha1
kind: Skill
metadata:
  name: weather
services:
  demo:
    baseURL: https://example.com
operations:
  current:
    service: demo
    method: GET
    path: /weather
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	add, err := declarativeFileRequest("add", path, dir)
	if err != nil {
		t.Fatal(err)
	}
	if add.Action != "declarative.add" || add.Source != source || add.Root != dir {
		t.Fatalf("add=%#v", add)
	}
	diff, err := declarativeFileRequest("diff", path, dir)
	if err != nil || diff.Action != "declarative.diff" {
		t.Fatalf("diff=%#v err=%v", diff, err)
	}
	remove := declarativeRemoveRequest("weather")
	if remove.Action != "declarative.remove" || remove.Name != "weather" {
		t.Fatalf("remove=%#v", remove)
	}
	valid, err := validateDeclarativeFile(path)
	if err != nil || valid.Name != "weather" || len(valid.Capabilities) != 1 {
		t.Fatalf("valid=%#v err=%v", valid, err)
	}
}

func TestDeclarativeCommandsRejectInvalidSourceAndAction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("kind: Skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := declarativeFileRequest("add", path, t.TempDir()); err == nil {
		t.Fatal("invalid source accepted")
	}
	if _, err := declarativeFileRequest("unknown", path, t.TempDir()); err == nil {
		t.Fatal("unknown declarative action accepted")
	}
}

func TestCallsRequestExactCommands(t *testing.T) {
	tests := []struct {
		command     string
		target      string
		stdin       string
		after       int
		limit       int
		want        api.Request
		plainString bool
	}{
		{"start", "echo/demo/echo", `{"x":1}`, 0, 0, api.Request{Action: "call.start", Capability: "echo/demo/echo", Input: json.RawMessage(`{"x":1}`)}, true},
		{"start", "echo/demo/echo", "", 0, 0, api.Request{Action: "call.start", Capability: "echo/demo/echo", Input: json.RawMessage(`{}`)}, true},
		{"get", "call-1", "", 0, 0, api.Request{Action: "call.get", CallID: "call-1"}, false},
		{"events", "call-1", "", 0, 0, api.Request{Action: "call.events", CallID: "call-1"}, false},
		{"events", "call-1", "", 100, 25, api.Request{Action: "call.events", CallID: "call-1", After: 100, Limit: 25}, false},
		{"cancel", "call-1", "", 0, 0, api.Request{Action: "call.cancel", CallID: "call-1"}, false},
	}
	for _, test := range tests {
		got, plain, err := callsRequest(test.command, test.target, strings.NewReader(test.stdin), test.after, test.limit)
		if err != nil || plain != test.plainString || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("callsRequest(%s)=%#v plain=%v err=%v", test.command, got, plain, err)
		}
	}
}

func TestCallsRequestRejectsInvalidValues(t *testing.T) {
	for _, test := range []struct {
		command      string
		after, limit int
	}{{"events", -1, 0}, {"events", 0, -1}, {"unknown", 0, 0}} {
		if _, _, err := callsRequest(test.command, "call-1", strings.NewReader(""), test.after, test.limit); err == nil {
			t.Fatalf("callsRequest(%s, after=%d, limit=%d) accepted invalid values", test.command, test.after, test.limit)
		}
	}
}

func TestCallsRequestEnforcesAsyncPayloadBound(t *testing.T) {
	valid := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, callmodel.MaxPayloadBytes-2)...)
	valid = append(valid, '"')
	request, _, err := callsRequest("start", "echo/demo/echo", bytes.NewReader(valid), 0, 0)
	if err != nil || len(request.Input) != callmodel.MaxPayloadBytes {
		t.Fatalf("maximum calls input len=%d err=%v", len(request.Input), err)
	}
	oversized := append(valid, ' ')
	if _, _, err := callsRequest("start", "echo/demo/echo", bytes.NewReader(oversized), 0, 0); !errors.Is(err, callmodel.ErrPayloadTooLarge) || !strings.Contains(err.Error(), "payload_too_large") {
		t.Fatalf("oversized calls input error=%v", err)
	}
}

func TestInvokeRequestEnforcesSharedPayloadBound(t *testing.T) {
	valid := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, callmodel.MaxPayloadBytes-2)...)
	valid = append(valid, '"')
	request, err := invokeRequest("echo/demo/echo", bytes.NewReader(valid))
	if err != nil || len(request.Input) != callmodel.MaxPayloadBytes {
		t.Fatalf("maximum invoke input len=%d err=%v", len(request.Input), err)
	}
	oversized := append(valid, ' ')
	if _, err := invokeRequest("echo/demo/echo", bytes.NewReader(oversized)); !errors.Is(err, callmodel.ErrPayloadTooLarge) || !strings.Contains(err.Error(), "payload_too_large") {
		t.Fatalf("oversized invoke input error=%v", err)
	}
}

func TestInvocationInputRejectsMalformedJSON(t *testing.T) {
	if _, err := invokeRequest("echo/demo/echo", strings.NewReader(`{"missing":`)); err == nil || !strings.Contains(err.Error(), "valid JSON") {
		t.Fatalf("malformed JSON error=%v", err)
	}
}

func TestWorkspaceCommandRequests(t *testing.T) {
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		command         string
		action, backend string
		check, all      bool
	}{{"attach", "workspace.attach", "directory", false, false}, {"status", "workspace.status", "auto", false, false}, {"update", "workspace.update", "auto", true, false}, {"update", "workspace.update", "auto", false, true}, {"detach", "workspace.detach", "auto", false, false}}
	for _, test := range tests {
		request, err := workspaceCommandRequest(test.command, "", test.backend, root, test.check, test.all)
		if err != nil {
			t.Fatalf("%s: %v", test.command, err)
		}
		if request.Action != test.action || request.Root != canonical || request.Backend != test.backend || request.Check != test.check || request.All != test.all {
			t.Fatalf("%s: %#v", test.command, request)
		}
	}
	if _, err := workspaceCommandRequest("status", "", "fuse", root, false, false); err == nil {
		t.Fatal("status accepted backend")
	}
	if _, err := workspaceCommandRequest("detach", "", "auto", root, true, false); err == nil {
		t.Fatal("detach accepted check")
	}
	if _, err := workspaceCommandRequest("update", "", "auto", root, true, true); err == nil {
		t.Fatal("update accepted --check with --all")
	}
}

func TestWorkspaceForProjectionRoot(t *testing.T) {
	tests := map[string]string{"/work/.agents/skills": "/work", "/work/.claude/skills": "/work", "/tmp/custom-skills": "/tmp"}
	for root, want := range tests {
		if got := workspaceForProjectionRoot(root); got != want {
			t.Fatalf("workspaceForProjectionRoot(%q)=%q want %q", root, got, want)
		}
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

func TestCLIValidatesBeforeCallingDaemon(t *testing.T) {
	tests := []struct {
		name  string
		args  []string
		stdin string
		want  []string
	}{
		{"missing source", []string{"add"}, "", []string{"<source.yaml>"}},
		{"invalid backend", []string{"attach", "--backend", "memory"}, "", []string{"--backend", "auto", "fuse", "directory"}},
		{"conflicting update modes", []string{"update", "--check", "--all"}, "", []string{"check", "all"}},
		{"zero event limit", []string{"calls", "events", "call-1", "--limit", "0"}, "", []string{"--limit", "greater than zero"}},
		{"relative adapter", []string{"adapters", "add", "billing", "bin/adapter"}, "", []string{"absolute", "bin/adapter"}},
		{"malformed input", []string{"invoke", "echo/demo/echo"}, `{"missing":`, []string{"valid JSON"}},
		{"invalid search format", []string{"search", "weather", "--format", "yaml"}, "", []string{"--format", "json"}},
		{"command typo", []string{"attch"}, "", []string{"Did you mean", "attach"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			c := &cli{
				cwd: t.TempDir(),
				call: func(api.Request) (json.RawMessage, error) {
					calls++
					return json.RawMessage("null"), nil
				},
				getenv: func(string) string { return "" },
			}
			_, err := executeTestCommand(t, c, test.args, test.stdin)
			if err == nil {
				t.Fatal("command accepted invalid input")
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q missing %q", err, want)
				}
			}
			if calls != 0 {
				t.Fatalf("daemon called %d times before validation completed", calls)
			}
		})
	}
}

func TestCLIMapsArgumentsToRequests(t *testing.T) {
	cwd := t.TempDir()
	canonical, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name       string
		args       []string
		stdin      string
		response   json.RawMessage
		want       api.Request
		wantStdout string
	}{
		{"status", []string{"status", "--workspace", cwd, "--json"}, "", json.RawMessage(`{"ok":true}`), api.Request{Action: "workspace.status", Root: canonical, Backend: "auto"}, "{\"ok\":true}\n"},
		{"search words", []string{"search", "weather", "temperature", "--format", "json"}, "", json.RawMessage(`{"ok":true}`), api.Request{Action: "search", Query: "weather temperature", Format: "json"}, "{\"ok\":true}\n"},
		{"invoke", []string{"invoke", "echo/demo/echo"}, `{"x":1}`, json.RawMessage(`{"ok":true}`), api.Request{Action: "invoke", Capability: "echo/demo/echo", Input: json.RawMessage(`{"x":1}`)}, "{\"ok\":true}\n"},
		{"events", []string{"calls", "events", "call-1", "--after", "100", "--limit", "25"}, "", json.RawMessage(`{"ok":true}`), api.Request{Action: "call.events", CallID: "call-1", After: 100, Limit: 25}, "{\"ok\":true}\n"},
		{"permissions", []string{"acl", "grant", "agent", "echo/demo/echo", "read, invoke"}, "", json.RawMessage("null"), api.Request{Action: "acl.grant", Identity: "agent", Capability: "echo/demo/echo", Permissions: []string{"read", "invoke"}}, ""},
		{"token", []string{"tokens", "create", "agent"}, "", json.RawMessage(`"secret"`), api.Request{Action: "token.create", Identity: "agent"}, "secret\n"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests []api.Request
			c := &cli{
				cwd: cwd,
				call: func(request api.Request) (json.RawMessage, error) {
					requests = append(requests, request)
					return test.response, nil
				},
				getenv: func(string) string { return "0" },
			}
			stdout, err := executeTestCommand(t, c, test.args, test.stdin)
			if err != nil {
				t.Fatal(err)
			}
			if len(requests) != 1 || !reflect.DeepEqual(requests[0], test.want) {
				t.Fatalf("requests=%#v want %#v", requests, test.want)
			}
			if stdout != test.wantStdout {
				t.Fatalf("stdout=%q want %q", stdout, test.wantStdout)
			}
		})
	}
}

func TestUpdateAutoAttachesTheSelectedWorkspace(t *testing.T) {
	workspace := t.TempDir()
	canonical, err := filepath.EvalSymlinks(workspace)
	if err != nil {
		t.Fatal(err)
	}
	var requests []api.Request
	c := &cli{
		cwd: t.TempDir(),
		call: func(request api.Request) (json.RawMessage, error) {
			requests = append(requests, request)
			return json.RawMessage("null"), nil
		},
		getenv: func(string) string { return "" },
	}
	if _, err := executeTestCommand(t, c, []string{"update", "--workspace", workspace}, ""); err != nil {
		t.Fatal(err)
	}
	want := []api.Request{
		{Action: "workspace.attach", Root: canonical, Backend: "auto"},
		{Action: "workspace.update", Root: canonical, Backend: "auto"},
	}
	if !reflect.DeepEqual(requests, want) {
		t.Fatalf("requests=%#v want %#v", requests, want)
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
		getenv: func(string) string { return "" },
	}
	stdout, err := executeTestCommand(t, c, []string{"completion", "bash"}, "")
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 || !strings.Contains(stdout, "__start_9a") {
		t.Fatalf("calls=%d completion=%q", calls, stdout)
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
			getenv: func(string) string { return "" },
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
		t.Fatalf("version command=%q flag=%q", commandOutput, flagOutput)
	}
}

func TestHelpDocumentsEveryCommandWithoutDaemon(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "9a")
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build 9a: %v\n%s", err, out)
	}

	tests := []struct {
		args []string
		want []string
	}{
		{nil, []string{"Workspace Commands:", "Skill Commands:", "Execution Commands:", "9a <command> --help"}},
		{[]string{"help", "attach"}, []string{"9a attach [flags]", "--workspace", "--backend"}},
		{[]string{"help", "status"}, []string{"9a status [flags]", "--workspace", "--json"}},
		{[]string{"help", "update"}, []string{"9a update [flags]", "--workspace", "--check", "--all"}},
		{[]string{"help", "detach"}, []string{"9a detach [flags]", "--workspace"}},
		{[]string{"help", "validate"}, []string{"9a validate <source.yaml>", "does not contact the daemon"}},
		{[]string{"help", "add"}, []string{"9a add <source.yaml>"}},
		{[]string{"help", "diff"}, []string{"9a diff <source.yaml>"}},
		{[]string{"help", "remove"}, []string{"9a remove <skill-name>"}},
		{[]string{"help", "calls", "start"}, []string{"9a calls start <capability>", "JSON from stdin"}},
		{[]string{"help", "calls", "get"}, []string{"9a calls get <call-id>"}},
		{[]string{"help", "calls", "events"}, []string{"9a calls events <call-id> [flags]", "--after", "--limit"}},
		{[]string{"help", "calls", "cancel"}, []string{"9a calls cancel <call-id>"}},
		{[]string{"help", "adapters", "add"}, []string{"9a adapters add <protocol> <absolute-executable>"}},
		{[]string{"help", "providers", "add"}, []string{"9a providers add <protocol> <name> <endpoint>"}},
		{[]string{"help", "providers", "remove"}, []string{"9a providers remove <protocol> <name>"}},
		{[]string{"help", "acl", "grant"}, []string{"9a acl grant <identity> <capability> <permissions>", "comma-separated"}},
		{[]string{"help", "tokens", "create"}, []string{"9a tokens create <identity>"}},
		{[]string{"help", "search"}, []string{"9a search <query...>", "--format"}},
		{[]string{"help", "project", "add"}, []string{"9a project add <capability> <skills-root>"}},
		{[]string{"help", "invoke"}, []string{"9a invoke <capability>", "JSON from stdin"}},
		{[]string{"help", "completion"}, []string{"9a completion <shell>", "bash", "zsh", "fish", "powershell"}},
		{[]string{"help", "version"}, []string{"9a version"}},
	}

	for _, test := range tests {
		command := exec.Command(binary, test.args...)
		command.Env = append(os.Environ(), "NINEA_SOCKET="+filepath.Join(t.TempDir(), "missing.sock"))
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("9a %s: %v\n%s", strings.Join(test.args, " "), err, out)
		}
		for _, want := range test.want {
			if !bytes.Contains(out, []byte(want)) {
				t.Fatalf("9a %s help missing %q:\n%s", strings.Join(test.args, " "), want, out)
			}
		}
	}
}
