package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/gopact-ai/9a/internal/api"
	callmodel "github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/declarative"
	workspacepkg "github.com/gopact-ai/9a/internal/workspace"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
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

func declarativeFileRequest(args []string, cwd string) (api.Request, error) {
	if len(args) != 2 || (args[0] != "add" && args[0] != "diff") {
		return api.Request{}, fmt.Errorf("usage: 9a <add|diff> <source.yaml>")
	}
	source, _, err := readDeclarativeFile(args[1])
	if err != nil {
		return api.Request{}, err
	}
	root, err := filepath.Abs(cwd)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "declarative." + args[0], Source: string(source), Root: root}, nil
}

func declarativeRemoveRequest(args []string) (api.Request, error) {
	if len(args) != 2 || args[0] != "remove" {
		return api.Request{}, fmt.Errorf("usage: 9a remove <skill-name>")
	}
	return api.Request{Action: "declarative.remove", Name: args[1]}, nil
}

func fail(v ...any) { fmt.Fprintln(os.Stderr, v...); os.Exit(1) }

func workspaceCommandRequest(args []string, cwd string) (api.Request, error) {
	if len(args) == 0 {
		return api.Request{}, fmt.Errorf("usage: 9a <attach|status|update|detach> [--workspace <path>] [--backend <auto|fuse|directory>]")
	}
	command := args[0]
	if command != "attach" && command != "status" && command != "update" && command != "detach" {
		return api.Request{}, fmt.Errorf("unknown workspace command")
	}
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	explicit := flags.String("workspace", "", "")
	backend := flags.String("backend", "auto", "")
	_ = flags.Bool("json", false, "")
	check := flags.Bool("check", false, "")
	all := flags.Bool("all", false, "")
	if err := flags.Parse(args[1:]); err != nil {
		return api.Request{}, err
	}
	if flags.NArg() != 0 {
		return api.Request{}, fmt.Errorf("unexpected arguments")
	}
	if command != "attach" && *backend != "auto" {
		return api.Request{}, fmt.Errorf("--backend is only valid with attach")
	}
	root, err := workspacepkg.Resolve(*explicit, cwd)
	if err != nil {
		return api.Request{}, err
	}
	action := map[string]string{"attach": "workspace.attach", "status": "workspace.status", "update": "workspace.update", "detach": "workspace.detach"}[command]
	if command != "update" && (*check || *all) {
		return api.Request{}, fmt.Errorf("--check and --all are only valid with update")
	}
	return api.Request{Action: action, Root: root, Backend: *backend, Check: *check, All: *all}, nil
}

func autoAttach(command string) bool {
	switch command {
	case "search", "project", "add", "providers", "adapters", "update":
		return true
	}
	return false
}

func adapterAddRequest(args []string) (api.Request, error) {
	if len(args) != 4 || args[1] != "add" {
		return api.Request{}, fmt.Errorf("usage: 9a adapters add <protocol> <absolute-executable>")
	}
	return api.Request{Action: "adapter.add", Protocol: args[2], Executable: args[3]}, nil
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
	return input, nil
}

func invokeRequest(args []string, stdin io.Reader) (api.Request, error) {
	if len(args) != 2 {
		return api.Request{}, fmt.Errorf("usage: invoke <capability>")
	}
	input, err := readInvocationInput(stdin)
	if err != nil {
		return api.Request{}, err
	}
	return api.Request{Action: "invoke", Capability: args[1], Input: input}, nil
}

func callsRequest(args []string, stdin io.Reader) (api.Request, bool, error) {
	usage := "usage: 9a calls <start <capability>|get <call-id>|events <call-id> [--after <sequence>] [--limit <count>]|cancel <call-id>>"
	if len(args) < 3 || (args[1] != "events" && len(args) != 3) {
		return api.Request{}, false, fmt.Errorf("%s", usage)
	}
	switch args[1] {
	case "start":
		input, err := readInvocationInput(stdin)
		if err != nil {
			return api.Request{}, false, err
		}
		return api.Request{Action: "call.start", Capability: args[2], Input: input}, true, nil
	case "get":
		return api.Request{Action: "call.get", CallID: args[2]}, false, nil
	case "events":
		request := api.Request{Action: "call.events", CallID: args[2]}
		if (len(args)-3)%2 != 0 {
			return api.Request{}, false, fmt.Errorf("%s", usage)
		}
		seen := map[string]bool{}
		for i := 3; i < len(args); i += 2 {
			flag := args[i]
			value, err := strconv.Atoi(args[i+1])
			if err != nil || seen[flag] {
				return api.Request{}, false, fmt.Errorf("%s", usage)
			}
			seen[flag] = true
			switch flag {
			case "--after":
				if value < 0 {
					return api.Request{}, false, fmt.Errorf("%s", usage)
				}
				request.After = value
			case "--limit":
				if value <= 0 {
					return api.Request{}, false, fmt.Errorf("%s", usage)
				}
				request.Limit = value
			default:
				return api.Request{}, false, fmt.Errorf("%s", usage)
			}
		}
		return request, false, nil
	case "cancel":
		return api.Request{Action: "call.cancel", CallID: args[2]}, false, nil
	default:
		return api.Request{}, false, fmt.Errorf("%s", usage)
	}
}

func call(q api.Request) json.RawMessage {
	socket := os.Getenv("NINEA_SOCKET")
	if socket == "" {
		socket = "/tmp/ninea.sock"
	}
	body, _ := json.Marshal(q)
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", socket)
	}}
	req, _ := http.NewRequest("POST", "http://unix/rpc", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+os.Getenv("NINEA_TOKEN"))
	resp, e := (&http.Client{Transport: tr, Timeout: 30 * time.Second}).Do(req)
	if e != nil {
		fail(e)
	}
	defer resp.Body.Close()
	var out struct {
		Data  json.RawMessage `json:"data"`
		Error string          `json:"error"`
		Code  string          `json:"code"`
	}
	if e = json.NewDecoder(resp.Body).Decode(&out); e != nil {
		fail(e)
	}
	if resp.StatusCode >= 300 || out.Error != "" {
		if len(out.Data) > 0 && string(out.Data) != "null" {
			_, _ = os.Stderr.Write(out.Data)
			_, _ = os.Stderr.Write([]byte("\n"))
		}
		fail(out.Code + ": " + out.Error)
	}
	return out.Data
}
func main() {
	a := os.Args[1:]
	if len(a) == 0 {
		fail("usage: 9a <command>")
	}
	var q api.Request
	plainString := false
	cwd, cwdErr := os.Getwd()
	if cwdErr != nil {
		fail(cwdErr)
	}
	skipAttach := (a[0] == "update" && (slices.Contains(a, "--check") || slices.Contains(a, "--all"))) || (a[0] == "providers" && len(a) > 1 && a[1] == "remove")
	if autoAttach(a[0]) && !skipAttach && os.Getenv("NINEA_AUTO_ATTACH") != "0" {
		root, err := workspacepkg.Resolve("", cwd)
		if err != nil {
			fail(err)
		}
		_ = call(api.Request{Action: "workspace.attach", Root: root, Backend: "auto"})
	}
	switch a[0] {
	case "attach", "status", "update", "detach":
		request, err := workspaceCommandRequest(a, cwd)
		if err != nil {
			fail(err)
		}
		q = request
	case "validate":
		if len(a) != 2 {
			fail("usage: 9a validate <source.yaml>")
		}
		result, err := validateDeclarativeFile(a[1])
		if err != nil {
			fail(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fail(err)
		}
		return
	case "add", "diff":
		request, err := declarativeFileRequest(a, cwd)
		if err != nil {
			fail(err)
		}
		q = request
	case "remove":
		request, err := declarativeRemoveRequest(a)
		if err != nil {
			fail(err)
		}
		q = request
	case "calls":
		request, plain, err := callsRequest(a, os.Stdin)
		if err != nil {
			fail(err)
		}
		q = request
		plainString = plain
	case "adapters":
		request, err := adapterAddRequest(a)
		if err != nil {
			fail(err)
		}
		q = request
	case "providers":
		if len(a) == 4 && a[1] == "remove" {
			q = api.Request{Action: "provider.remove", Protocol: a[2], Name: a[3]}
			break
		}
		if len(a) != 5 || a[1] != "add" {
			fail("usage: providers <add <protocol> <name> <endpoint>|remove <protocol> <name>>")
		}
		q = api.Request{Action: "provider.add", Protocol: a[2], Name: a[3], Endpoint: a[4]}
	case "acl":
		if len(a) != 5 || a[1] != "grant" {
			fail("usage: acl grant <identity> <capability> <permissions>")
		}
		q = api.Request{Action: "acl.grant", Identity: a[2], Capability: a[3], Permissions: strings.Split(a[4], ",")}
	case "tokens":
		if len(a) != 3 || a[1] != "create" {
			fail("usage: tokens create <identity>")
		}
		q = api.Request{Action: "token.create", Identity: a[2]}
		plainString = true
	case "search":
		if len(a) < 2 {
			fail("usage: search <query>")
		}
		q = api.Request{Action: "search", Query: a[1]}
	case "project":
		if len(a) != 4 || a[1] != "add" {
			fail("usage: project add <capability> <root>")
		}
		root, err := filepath.Abs(a[3])
		if err != nil {
			fail(err)
		}
		workspaceRoot, err := workspacepkg.Resolve("", cwd)
		if err != nil {
			fail(err)
		}
		if os.Getenv("NINEA_AUTO_ATTACH") == "0" {
			workspaceRoot = filepath.Dir(root)
		}
		q = api.Request{Action: "project.add", Capability: a[2], Workspace: workspaceRoot, Root: root}
	case "invoke":
		request, err := invokeRequest(a, os.Stdin)
		if err != nil {
			fail(err)
		}
		q = request
	default:
		fail("unknown command")
	}
	data := call(q)
	if plainString {
		var value string
		if err := json.Unmarshal(data, &value); err != nil {
			fail(err)
		}
		fmt.Fprintln(os.Stdout, value)
		return
	}
	if len(data) > 0 && string(data) != "null" {
		_, _ = os.Stdout.Write(data)
		_, _ = os.Stdout.Write([]byte("\n"))
	}
}
