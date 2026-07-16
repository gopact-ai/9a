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
	"os/signal"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gopact-ai/9a/internal/api"
	callmodel "github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/secret"
	workspacepkg "github.com/gopact-ai/9a/internal/workspace"
	"golang.org/x/term"
	"gopkg.in/yaml.v3"
)

const maxSecretBytes = secret.MaxValueBytes

func readManifestFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	source, err := io.ReadAll(io.LimitReader(file, declarative.MaxSourceBytes+1))
	closeErr := file.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	if len(source) > declarative.MaxSourceBytes {
		return nil, fmt.Errorf("source exceeds %d bytes", declarative.MaxSourceBytes)
	}
	if _, err := declarative.Parse(source); err != nil {
		return nil, err
	}
	return source, nil
}

func connectRequest(path, cwd string) (api.Request, error) {
	source, err := readManifestFile(path)
	if err != nil {
		return api.Request{}, err
	}
	root, err := workspacepkg.Resolve("", cwd)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "connect", Source: string(source), Root: root}, nil
}

type protocolManifest struct {
	Version    int    `yaml:"version"`
	Name       string `yaml:"name"`
	Type       string `yaml:"type"`
	Executable string `yaml:"executable,omitempty"`
	URL        string `yaml:"url,omitempty"`
}

func protocolConnectRequest(protocol, name, target, cwd string) (api.Request, error) {
	if name == "" {
		return api.Request{}, fmt.Errorf("--name must be a canonical non-empty slug")
	}
	if err := validateIntegrationName(name); err != nil {
		return api.Request{}, err
	}
	manifest := protocolManifest{Version: 1, Name: name, Type: protocol}
	switch protocol {
	case "mcp":
		manifest.Executable = target
	case "a2a":
		manifest.URL = target
	default:
		return api.Request{}, fmt.Errorf("unsupported integration type %q", protocol)
	}
	source, err := yaml.Marshal(manifest)
	if err != nil {
		return api.Request{}, fmt.Errorf("encode manifest: %w", err)
	}
	if _, err := declarative.Parse(source); err != nil {
		return api.Request{}, err
	}
	root, err := workspacepkg.Resolve("", cwd)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "connect", Source: string(source), Root: root}, nil
}

func statusRequest(workspace, cwd, integration string) (api.Request, error) {
	root, err := workspacepkg.Resolve(workspace, cwd)
	if err != nil {
		return api.Request{}, err
	}
	if err := validateIntegrationName(integration); err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "status", Root: root, Name: integration}, nil
}

func doctorRequest(workspace, cwd string, fix bool) (api.Request, error) {
	root, err := workspacepkg.Resolve(workspace, cwd)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "doctor", Root: root, Fix: fix}, nil
}

func disconnectRequest(name, cwd string) (api.Request, error) {
	if name == "" {
		return api.Request{}, fmt.Errorf("integration must be a canonical non-empty slug")
	}
	if err := validateIntegrationName(name); err != nil {
		return api.Request{}, err
	}
	root, err := workspacepkg.Resolve("", cwd)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "disconnect", Name: name, Root: root}, nil
}

func secretSetRequest(reference string, reader io.Reader, errorOutput io.Writer) (api.Request, error) {
	if err := secret.ValidateReference(reference); err != nil {
		return api.Request{}, err
	}
	value, err := readSecretValue(reader, errorOutput, reference)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "secret.set", Name: reference, Value: value}, nil
}

func secretListRequest(integration string) (api.Request, error) {
	if err := validateIntegrationName(integration); err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "secret.list", Name: integration}, nil
}

func validateIntegrationName(integration string) error {
	if integration != "" && (strings.TrimSpace(integration) != integration || secret.ValidateReference(integration+".value") != nil) {
		return fmt.Errorf("integration must be a canonical non-empty slug")
	}
	return nil
}

func secretUnsetRequest(reference string) (api.Request, error) {
	if err := secret.ValidateReference(reference); err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "secret.unset", Name: reference}, nil
}

func readSecretValue(reader io.Reader, errorOutput io.Writer, reference string) (string, error) {
	if file, ok := reader.(*os.File); ok && term.IsTerminal(int(file.Fd())) {
		if _, err := fmt.Fprintf(errorOutput, "Secret for %s: ", reference); err != nil {
			return "", err
		}
		value, err := term.ReadPassword(int(file.Fd()))
		_, newlineErr := fmt.Fprintln(errorOutput)
		if err != nil {
			return "", fmt.Errorf("read secret: %w", err)
		}
		if newlineErr != nil {
			return "", newlineErr
		}
		return validateSecretValue(value)
	}
	value, err := io.ReadAll(io.LimitReader(reader, maxSecretBytes+1))
	if err != nil {
		return "", fmt.Errorf("read secret: %w", err)
	}
	value = bytes.TrimSuffix(value, []byte("\n"))
	value = bytes.TrimSuffix(value, []byte("\r"))
	return validateSecretValue(value)
}

func validateSecretValue(value []byte) (string, error) {
	if len(value) == 0 {
		return "", fmt.Errorf("secret value is empty")
	}
	if len(value) > maxSecretBytes {
		return "", fmt.Errorf("secret value exceeds %d bytes", maxSecretBytes)
	}
	if !utf8.Valid(value) {
		return "", fmt.Errorf("secret value is not valid UTF-8")
	}
	return string(value), nil
}

func readInput(reader io.Reader) ([]byte, error) {
	input, err := io.ReadAll(io.LimitReader(reader, callmodel.MaxPayloadBytes+1))
	if err != nil {
		return nil, err
	}
	if len(input) > callmodel.MaxPayloadBytes {
		return nil, fmt.Errorf("payload_too_large: %w", callmodel.ErrPayloadTooLarge)
	}
	return input, nil
}

func readOptionalStdin(stdin io.Reader) ([]byte, error) {
	if file, ok := stdin.(*os.File); ok {
		info, err := file.Stat()
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeCharDevice != 0 {
			return nil, nil
		}
	}
	return readInput(stdin)
}

func capabilityRunRequest(capability, flagInput string, stdin io.Reader) (api.Request, error) {
	parts := strings.Split(capability, "/")
	if len(parts) != 2 || strings.TrimSpace(parts[0]) != parts[0] || strings.TrimSpace(parts[1]) != parts[1] || parts[0] == "" || parts[1] == "" || secret.ValidateReference(parts[0]+"."+parts[1]) != nil {
		return api.Request{}, fmt.Errorf("capability must use <integration>/<capability>")
	}
	trimmedFlagInput := strings.TrimSpace(flagInput)
	if trimmedFlagInput == "-" {
		input, err := readInput(stdin)
		if err != nil {
			return api.Request{}, fmt.Errorf("read stdin: %w", err)
		}
		return runRequestWithInput(capability, input)
	}

	stdinInput, err := readOptionalStdin(stdin)
	if err != nil {
		return api.Request{}, fmt.Errorf("read stdin: %w", err)
	}
	if trimmedFlagInput != "" && len(bytes.TrimSpace(stdinInput)) > 0 {
		return api.Request{}, fmt.Errorf("provide JSON through only one of --input or stdin")
	}
	input := stdinInput
	if trimmedFlagInput != "" {
		if strings.HasPrefix(trimmedFlagInput, "@") {
			path := strings.TrimPrefix(trimmedFlagInput, "@")
			if path == "" {
				return api.Request{}, fmt.Errorf("--input @path requires a file path")
			}
			file, err := os.Open(path)
			if err != nil {
				return api.Request{}, fmt.Errorf("read input file %q: %w", path, err)
			}
			input, err = readInput(file)
			closeErr := file.Close()
			if err != nil {
				return api.Request{}, fmt.Errorf("read input file %q: %w", path, err)
			}
			if closeErr != nil {
				return api.Request{}, fmt.Errorf("close input file %q: %w", path, closeErr)
			}
		} else {
			input, err = readInput(strings.NewReader(flagInput))
			if err != nil {
				return api.Request{}, err
			}
		}
	}
	return runRequestWithInput(capability, input)
}

func runRequestWithInput(capability string, input []byte) (api.Request, error) {
	if len(bytes.TrimSpace(input)) == 0 {
		input = []byte("{}")
	}
	if !json.Valid(input) {
		return api.Request{}, fmt.Errorf("input must contain one valid JSON value")
	}
	return api.Request{Action: "run", Capability: capability, Input: input}, nil
}

type rpcError struct {
	code           string
	message        string
	data           json.RawMessage
	machineWritten bool
}

type rpcTransportError struct {
	action             string
	requestMayHaveSent bool
	err                error
}

func (e *rpcTransportError) Error() string {
	if e.action == "run" && e.requestMayHaveSent {
		return "local runtime connection failed after the run may have started; check upstream state before retrying: " + e.err.Error()
	}
	return "local runtime connection failed: " + e.err.Error()
}

func (e *rpcTransportError) Unwrap() error { return e.err }

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

func doRPC(ctx context.Context, body []byte, socket, token, action string) (json.RawMessage, error) {
	var connected atomic.Bool
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		connection, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
		if err == nil {
			connected.Store(true)
		}
		return connection, err
	}}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://unix/rpc", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return nil, &rpcTransportError{action: action, requestMayHaveSent: connected.Load(), err: err}
	}
	var out struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
		Code  string          `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		_ = resp.Body.Close()
		return nil, &rpcTransportError{action: action, requestMayHaveSent: true, err: err}
	}
	if err := resp.Body.Close(); err != nil {
		return nil, &rpcTransportError{action: action, requestMayHaveSent: true, err: err}
	}
	if resp.StatusCode >= http.StatusMultipleChoices || out.Error != "" {
		if out.Error == "" {
			out.Error = resp.Status
		}
		return nil, &rpcError{code: out.Code, message: out.Error, data: out.Data}
	}
	return out.Data, nil
}

func callRPCContext(ctx context.Context, q api.Request) (json.RawMessage, error) {
	paths, err := defaultLocalPaths()
	if err != nil {
		return nil, &rpcTransportError{action: q.Action, err: err}
	}
	socket, token, err := localRPCConfig(os.Getenv, paths)
	if err != nil {
		return nil, &rpcTransportError{action: q.Action, err: err}
	}
	body, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	data, err := doRPC(ctx, body, socket, token, q.Action)
	var transportErr *rpcTransportError
	if !errors.As(err, &transportErr) || transportErr.requestMayHaveSent || !daemonUnavailable(err) {
		return data, err
	}
	if err := startLocalDaemon(paths, socket); err != nil {
		return nil, &rpcTransportError{action: q.Action, err: fmt.Errorf("start local daemon: %w; log: %s", err, paths.log)}
	}
	_, token, err = localRPCConfig(os.Getenv, paths)
	if err != nil {
		return nil, &rpcTransportError{action: q.Action, err: err}
	}
	return doRPC(ctx, body, socket, token, q.Action)
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	executed, err := root.ExecuteContextC(ctx)
	if err != nil {
		if executed == nil {
			executed = root
		}
		var remote *rpcError
		if wantsJSON(executed) {
			if !errors.As(err, &remote) || !remote.machineWritten {
				if outputErr := writeTopLevelMachineError(executed, err); outputErr != nil {
					fmt.Fprintln(os.Stderr, "Error:", outputErr)
				}
			}
			os.Exit(1)
		}
		if errors.As(err, &remote) && len(remote.data) > 0 && string(remote.data) != "null" {
			_, _ = os.Stderr.Write(remote.data)
			_, _ = os.Stderr.Write([]byte("\n"))
		}
		fmt.Fprintln(os.Stderr, "Error:", err)
		os.Exit(1)
	}
}
