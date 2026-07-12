package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gopact-ai/9a/internal/api"
	callmodel "github.com/gopact-ai/9a/internal/call"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

func fail(v ...any) { fmt.Fprintln(os.Stderr, v...); os.Exit(1) }

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
	switch a[0] {
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
		if len(a) != 5 || a[1] != "add" {
			fail("usage: providers add <protocol> <name> <endpoint>")
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
		q = api.Request{Action: "project.add", Capability: a[2], Root: a[3]}
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
