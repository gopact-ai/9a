package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gopact-ai/9a/internal/api"
)

func TestProtocolConnectRequestBuildsValidatedManifest(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "mcp-server")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		protocol string
		name     string
		target   string
		source   string
	}{
		{
			protocol: "mcp",
			name:     "local-tools",
			target:   executable,
			source:   "version: 1\nname: local-tools\ntype: mcp\nexecutable: " + executable + "\n",
		},
		{
			protocol: "a2a",
			name:     "research-agent",
			target:   "https://agent.example.com",
			source:   "version: 1\nname: research-agent\ntype: a2a\nurl: https://agent.example.com\n",
		},
	}
	for _, test := range tests {
		t.Run(test.protocol, func(t *testing.T) {
			got, err := protocolConnectRequest(test.protocol, test.name, test.target, dir)
			if err != nil {
				t.Fatal(err)
			}
			want := api.Request{Action: "connect", Source: test.source, Root: root}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("protocolConnectRequest() = %#v, want %#v", got, want)
			}
		})
	}
}

func TestProtocolConnectRequestRejectsInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "mcp-server")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name     string
		protocol string
		provider string
		target   string
	}{
		{name: "missing name", protocol: "mcp", target: executable},
		{name: "noncanonical name", protocol: "mcp", provider: "LocalTools", target: executable},
		{name: "relative executable", protocol: "mcp", provider: "local-tools", target: "mcp-server"},
		{name: "unsafe agent URL", protocol: "a2a", provider: "research-agent", target: "http://agent.example.com"},
		{name: "unknown protocol", protocol: "smtp", provider: "mail", target: "smtp://example.com"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := protocolConnectRequest(test.protocol, test.provider, test.target, dir); err == nil {
				t.Fatal("protocolConnectRequest accepted invalid input")
			}
		})
	}
}

func TestCLIConnectProtocolShortcutsUseOneConnectRequest(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "mcp-server")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		args   []string
		source string
	}{
		{
			name:   "mcp",
			args:   []string{"connect", "mcp", "--name", "local-tools", "--", executable},
			source: "version: 1\nname: local-tools\ntype: mcp\nexecutable: " + executable + "\n",
		},
		{
			name:   "a2a",
			args:   []string{"connect", "a2a", "--name", "research-agent", "https://agent.example.com"},
			source: "version: 1\nname: research-agent\ntype: a2a\nurl: https://agent.example.com\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests []api.Request
			c := &cli{
				cwd: dir,
				call: func(request api.Request) (json.RawMessage, error) {
					requests = append(requests, request)
					return json.RawMessage(`{"name":"` + test.name + `","capabilities":[]}`), nil
				},
			}
			if _, err := executeTestCommand(t, c, test.args, ""); err != nil {
				t.Fatal(err)
			}
			want := []api.Request{{Action: "connect", Source: test.source, Root: root}}
			if !reflect.DeepEqual(requests, want) {
				t.Fatalf("requests = %#v, want %#v", requests, want)
			}
		})
	}
}

func TestCLIConnectProtocolShortcutsValidateBeforeCallingDaemon(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
	}{
		{name: "mcp missing name", args: []string{"connect", "mcp", "--", "/bin/sh"}},
		{name: "mcp arguments", args: []string{"connect", "mcp", "--name", "local-tools", "--", "/bin/sh", "--serve"}},
		{name: "a2a missing name", args: []string{"connect", "a2a", "https://agent.example.com"}},
		{name: "a2a arguments", args: []string{"connect", "a2a", "--name", "research-agent", "https://one.example.com", "https://two.example.com"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			c := &cli{cwd: t.TempDir(), call: func(api.Request) (json.RawMessage, error) {
				calls++
				return nil, nil
			}}
			if _, err := executeTestCommand(t, c, test.args, ""); err == nil {
				t.Fatal("command accepted invalid arguments")
			}
			if calls != 0 {
				t.Fatalf("daemon called %d times", calls)
			}
		})
	}
}

func TestConnectHelpDocumentsProtocolShortcuts(t *testing.T) {
	c := &cli{cwd: t.TempDir()}
	for _, test := range []struct {
		args []string
		want []string
	}{
		{args: []string{"connect", "--help"}, want: []string{"9a connect [manifest.yaml]", "--guide", "mcp", "a2a"}},
		{args: []string{"connect", "mcp", "--help"}, want: []string{"9a connect mcp --name <slug> -- /absolute/executable", "no arguments"}},
		{args: []string{"connect", "a2a", "--help"}, want: []string{"9a connect a2a --name <slug> <url>", "HTTPS"}},
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
