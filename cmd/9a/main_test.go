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
	got, err := adapterAddRequest([]string{"adapters", "add", "billing", "/opt/ninea/billing-adapter"})
	if err != nil {
		t.Fatal(err)
	}
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
	add, err := declarativeFileRequest([]string{"add", path}, dir)
	if err != nil {
		t.Fatal(err)
	}
	if add.Action != "declarative.add" || add.Source != source || add.Root != dir {
		t.Fatalf("add=%#v", add)
	}
	diff, err := declarativeFileRequest([]string{"diff", path}, dir)
	if err != nil || diff.Action != "declarative.diff" {
		t.Fatalf("diff=%#v err=%v", diff, err)
	}
	remove, err := declarativeRemoveRequest([]string{"remove", "weather"})
	if err != nil || remove.Action != "declarative.remove" || remove.Name != "weather" {
		t.Fatalf("remove=%#v err=%v", remove, err)
	}
	valid, err := validateDeclarativeFile(path)
	if err != nil || valid.Name != "weather" || len(valid.Capabilities) != 1 {
		t.Fatalf("valid=%#v err=%v", valid, err)
	}
}

func TestDeclarativeCommandsRejectInvalidSourceAndUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	if err := os.WriteFile(path, []byte("kind: Skill\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := declarativeFileRequest([]string{"add", path}, t.TempDir()); err == nil {
		t.Fatal("invalid source accepted")
	}
	if _, err := declarativeRemoveRequest([]string{"remove"}); err == nil {
		t.Fatal("invalid remove usage accepted")
	}
}

func TestAdapterAddRequestExactUsage(t *testing.T) {
	for _, args := range [][]string{
		{"adapters"},
		{"adapters", "list"},
		{"adapters", "add", "billing"},
		{"adapters", "add", "billing", "/bin/true", "extra"},
	} {
		if _, err := adapterAddRequest(args); err == nil || err.Error() != "usage: 9a adapters add <protocol> <absolute-executable>" {
			t.Fatalf("adapterAddRequest(%v) error=%v", args, err)
		}
	}
}

func TestCallsRequestExactCommands(t *testing.T) {
	tests := []struct {
		args        []string
		stdin       string
		want        api.Request
		plainString bool
	}{
		{[]string{"calls", "start", "echo/demo/echo"}, `{"x":1}`, api.Request{Action: "call.start", Capability: "echo/demo/echo", Input: json.RawMessage(`{"x":1}`)}, true},
		{[]string{"calls", "start", "echo/demo/echo"}, "", api.Request{Action: "call.start", Capability: "echo/demo/echo", Input: json.RawMessage(`{}`)}, true},
		{[]string{"calls", "get", "call-1"}, "", api.Request{Action: "call.get", CallID: "call-1"}, false},
		{[]string{"calls", "events", "call-1"}, "", api.Request{Action: "call.events", CallID: "call-1"}, false},
		{[]string{"calls", "events", "call-1", "--after", "100", "--limit", "25"}, "", api.Request{Action: "call.events", CallID: "call-1", After: 100, Limit: 25}, false},
		{[]string{"calls", "cancel", "call-1"}, "", api.Request{Action: "call.cancel", CallID: "call-1"}, false},
	}
	for _, test := range tests {
		got, plain, err := callsRequest(test.args, strings.NewReader(test.stdin))
		if err != nil || plain != test.plainString || !reflect.DeepEqual(got, test.want) {
			t.Fatalf("callsRequest(%v)=%#v plain=%v err=%v", test.args, got, plain, err)
		}
	}
}

func TestCallsRequestExactUsage(t *testing.T) {
	for _, args := range [][]string{{"calls"}, {"calls", "start"}, {"calls", "get", "call-1", "extra"}, {"calls", "events", "call-1", "--after", "bad"}, {"calls", "events", "call-1", "--limit", "0"}, {"calls", "unknown", "call-1"}} {
		if _, _, err := callsRequest(args, strings.NewReader("")); err == nil || err.Error() != "usage: 9a calls <start <capability>|get <call-id>|events <call-id> [--after <sequence>] [--limit <count>]|cancel <call-id>>" {
			t.Fatalf("callsRequest(%v) error=%v", args, err)
		}
	}
}

func TestCallsRequestEnforcesAsyncPayloadBound(t *testing.T) {
	valid := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, callmodel.MaxPayloadBytes-2)...)
	valid = append(valid, '"')
	request, _, err := callsRequest([]string{"calls", "start", "echo/demo/echo"}, bytes.NewReader(valid))
	if err != nil || len(request.Input) != callmodel.MaxPayloadBytes {
		t.Fatalf("maximum calls input len=%d err=%v", len(request.Input), err)
	}
	oversized := append(valid, ' ')
	if _, _, err := callsRequest([]string{"calls", "start", "echo/demo/echo"}, bytes.NewReader(oversized)); !errors.Is(err, callmodel.ErrPayloadTooLarge) || !strings.Contains(err.Error(), "payload_too_large") {
		t.Fatalf("oversized calls input error=%v", err)
	}
}

func TestInvokeRequestEnforcesSharedPayloadBound(t *testing.T) {
	valid := append([]byte{'"'}, bytes.Repeat([]byte{'x'}, callmodel.MaxPayloadBytes-2)...)
	valid = append(valid, '"')
	request, err := invokeRequest([]string{"invoke", "echo/demo/echo"}, bytes.NewReader(valid))
	if err != nil || len(request.Input) != callmodel.MaxPayloadBytes {
		t.Fatalf("maximum invoke input len=%d err=%v", len(request.Input), err)
	}
	oversized := append(valid, ' ')
	if _, err := invokeRequest([]string{"invoke", "echo/demo/echo"}, bytes.NewReader(oversized)); !errors.Is(err, callmodel.ErrPayloadTooLarge) || !strings.Contains(err.Error(), "payload_too_large") {
		t.Fatalf("oversized invoke input error=%v", err)
	}
}

func TestWorkspaceCommandRequests(t *testing.T) {
	root := t.TempDir()
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		args            []string
		action, backend string
		check, all      bool
	}{{[]string{"attach", "--backend", "directory"}, "workspace.attach", "directory", false, false}, {[]string{"status", "--json"}, "workspace.status", "auto", false, false}, {[]string{"update", "--check"}, "workspace.update", "auto", true, false}, {[]string{"update", "--all"}, "workspace.update", "auto", false, true}, {[]string{"detach"}, "workspace.detach", "auto", false, false}}
	for _, test := range tests {
		request, err := workspaceCommandRequest(test.args, root)
		if err != nil {
			t.Fatalf("%v: %v", test.args, err)
		}
		if request.Action != test.action || request.Root != canonical || request.Backend != test.backend || request.Check != test.check || request.All != test.all {
			t.Fatalf("%v: %#v", test.args, request)
		}
	}
	if _, err := workspaceCommandRequest([]string{"status", "--backend", "fuse"}, root); err == nil {
		t.Fatal("status accepted backend")
	}
	if _, err := workspaceCommandRequest([]string{"detach", "--check"}, root); err == nil {
		t.Fatal("detach accepted check")
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

func TestHelpListsCommandsWithoutDaemon(t *testing.T) {
	out, err := exec.Command("go", "run", ".", "help").CombinedOutput()
	if err != nil {
		t.Fatalf("9a help: %v\n%s", err, out)
	}
	for _, want := range []string{"Usage: 9a <command>", "attach", "validate", "invoke"} {
		if !bytes.Contains(out, []byte(want)) {
			t.Fatalf("9a help output missing %q:\n%s", want, out)
		}
	}
}
