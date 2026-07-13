package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/gopact-ai/9a/internal/api"
	callmodel "github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/declarative"
	workspacepkg "github.com/gopact-ai/9a/internal/workspace"
)

type validationResult struct {
	Valid        bool     `json:"valid"`
	Name         string   `json:"name"`
	Digest       string   `json:"digest"`
	Capabilities []string `json:"capabilities"`
}

func readDeclarativeFile(path string) ([]byte, *declarative.Config, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer file.Close()
	source, err := io.ReadAll(io.LimitReader(file, declarative.MaxSourceBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(source) > declarative.MaxSourceBytes {
		return nil, nil, fmt.Errorf("source exceeds %d bytes", declarative.MaxSourceBytes)
	}
	config, err := declarative.Parse(source)
	if err != nil {
		return nil, nil, err
	}
	return source, config, nil
}

func validateDeclarativeFile(path string) (validationResult, error) {
	_, config, err := readDeclarativeFile(path)
	if err != nil {
		return validationResult{}, err
	}
	capabilities := make([]string, 0, len(config.Operations)+len(config.Workflows))
	for name := range config.Operations {
		capabilities = append(capabilities, "api/"+config.Metadata.Name+"/"+name)
	}
	for name := range config.Workflows {
		capabilities = append(capabilities, "api/"+config.Metadata.Name+"/"+name)
	}
	sort.Strings(capabilities)
	return validationResult{Valid: true, Name: config.Metadata.Name, Digest: config.Digest, Capabilities: capabilities}, nil
}

func declarativeFileRequest(action, path, cwd string) (api.Request, error) {
	if action != "add" && action != "diff" {
		return api.Request{}, fmt.Errorf("unsupported declarative action %q", action)
	}
	source, _, err := readDeclarativeFile(path)
	if err != nil {
		return api.Request{}, err
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "declarative." + action, Source: string(source), Root: root}, nil
}

func declarativeRemoveRequest(name string) api.Request {
	return api.Request{Action: "declarative.remove", Name: name}
}

func workspaceCommandRequest(command, workspace, backend, cwd string, check, all bool) (api.Request, error) {
	if command != "attach" && command != "status" && command != "update" && command != "detach" {
		return api.Request{}, fmt.Errorf("unsupported workspace command %q", command)
	}
	if command != "attach" && backend != "auto" {
		return api.Request{}, fmt.Errorf("--backend is only valid with attach")
	}
	root, err := workspacepkg.Resolve(workspace, cwd)
	if err != nil {
		return api.Request{}, err
	}
	action := map[string]string{"attach": "workspace.attach", "status": "workspace.status", "update": "workspace.update", "detach": "workspace.detach"}[command]
	if command != "update" && (check || all) {
		return api.Request{}, fmt.Errorf("--check and --all are only valid with update")
	}
	if check && all {
		return api.Request{}, fmt.Errorf("--check and --all are mutually exclusive")
	}
	return api.Request{Action: action, Root: root, Backend: backend, Check: check, All: all}, nil
}

func workspaceForProjectionRoot(root string) string {
	clean := filepath.Clean(root)
	parent := filepath.Dir(clean)
	if filepath.Base(clean) == "skills" && (filepath.Base(parent) == ".agents" || filepath.Base(parent) == ".claude") {
		return filepath.Dir(parent)
	}
	return parent
}

func adapterAddRequest(protocol, executable string) api.Request {
	return api.Request{Action: "adapter.add", Protocol: protocol, Executable: executable}
}

func readInvocationInput(stdin io.Reader) (json.RawMessage, error) {
	input, err := io.ReadAll(io.LimitReader(stdin, callmodel.MaxPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(input) > callmodel.MaxPayloadBytes {
		return nil, fmt.Errorf("payload_too_large: %w", callmodel.ErrPayloadTooLarge)
	}
	if len(bytes.TrimSpace(input)) == 0 {
		input = []byte("{}")
	}
	if !json.Valid(input) {
		return nil, fmt.Errorf("stdin must contain one valid JSON value")
	}
	return input, nil
}

func invokeRequest(capability string, stdin io.Reader) (api.Request, error) {
	input, err := readInvocationInput(stdin)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "invoke", Capability: capability, Input: input}, nil
}

func callsRequest(command, target string, stdin io.Reader, after, limit int) (api.Request, bool, error) {
	switch command {
	case "start":
		input, err := readInvocationInput(stdin)
		if err != nil {
			return api.Request{}, false, err
		}
		return api.Request{Action: "call.start", Capability: target, Input: input}, true, nil
	case "get":
		return api.Request{Action: "call.get", CallID: target}, false, nil
	case "events":
		if after < 0 {
			return api.Request{}, false, fmt.Errorf("after sequence must be zero or greater")
		}
		if limit < 0 {
			return api.Request{}, false, fmt.Errorf("event limit must be zero or greater")
		}
		return api.Request{Action: "call.events", CallID: target, After: after, Limit: limit}, false, nil
	case "cancel":
		return api.Request{Action: "call.cancel", CallID: target}, false, nil
	default:
		return api.Request{}, false, fmt.Errorf("unsupported calls command %q", command)
	}
}

type rpcError struct {
	code    string
	message string
	data    json.RawMessage
}

func (e *rpcError) Error() string {
	if e.code == "" {
		return e.message
	}
	if e.message == "" {
		return e.code
	}
	return e.code + ": " + e.message
}

func localRPCConfig(getenv func(string) string, paths localPaths) (string, string, error) {
	socket := getenv("NINEA_SOCKET")
	if socket == "" {
		socket = paths.socket
	}
	token := getenv("NINEA_TOKEN")
	if token == "" {
		var err error
		token, err = loadToken(paths.token)
		if os.IsNotExist(err) {
			return socket, "", nil
		}
		if err != nil {
			return "", "", fmt.Errorf("read local admin token: %w", err)
		}
	}
	return socket, token, nil
}

func doRPC(body []byte, socket, token string) (json.RawMessage, error) {
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://unix/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Transport: transport, Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
		Code  string          `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	if resp.StatusCode >= http.StatusMultipleChoices || out.Error != "" {
		if out.Error == "" {
			out.Error = resp.Status
		}
		return nil, &rpcError{code: out.Code, message: out.Error, data: out.Data}
	}
	return out.Data, nil
}

func callRPC(q api.Request) (json.RawMessage, error) {
	paths, err := defaultLocalPaths()
	if err != nil {
		return nil, err
	}
	socket, token, err := localRPCConfig(os.Getenv, paths)
	if err != nil {
		return nil, err
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	data, err := doRPC(body, socket, token)
	if !daemonUnavailable(err) {
		return data, err
	}
	if err := startLocalDaemon(paths, socket); err != nil {
		return nil, fmt.Errorf("start local daemon: %w; log: %s", err, paths.log)
	}
	_, token, err = localRPCConfig(os.Getenv, paths)
	if err != nil {
		return nil, err
	}
	return doRPC(body, socket, token)
}

func main() {
	// Commands that need a workspace resolve it when they run. Ignoring Getwd
	// here keeps workspace-independent commands usable if the current directory
	// was removed and lets other commands report argument errors first.
	cwd, _ := os.Getwd()
	root := newRootCommand(newCLI(cwd))
	root.SetIn(os.Stdin)
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	if _, err := root.ExecuteC(); err != nil {
		var remote *rpcError
		if errors.As(err, &remote) && len(remote.data) > 0 && string(remote.data) != "null" {
			_, _ = os.Stderr.Write(remote.data)
			_, _ = os.Stderr.Write([]byte("\n"))
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
