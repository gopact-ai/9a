package main

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
)

const version = "9a.adapter/v1"

type request struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func main() {
	if path := os.Getenv("NINEA_ASYNC_FIXTURE_PIDS"); path != "" {
		file, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if file != nil {
			_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
			_ = file.Close()
		}
	}
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	held := ""
	for scanner.Scan() {
		var req request
		if json.Unmarshal(scanner.Bytes(), &req) != nil {
			os.Exit(2)
		}
		switch req.Method {
		case "discover":
			capability := map[string]any{
				"upstream_name": "async", "kind": "api.operation", "name": "Async", "description": "Async fixture",
				"input":  map[string]any{"mode": "json", "json_schema": map[string]any{"type": "object"}},
				"output": map[string]any{"mode": "json"}, "lifecycle": map[string]any{"sync": true, "streaming": true, "cancelable": true},
				"security": map[string]any{"requires_approval": "never", "upstream_auth": "adapter-configured"},
			}
			_ = encoder.Encode(map[string]any{"version": version, "id": req.ID, "result": map[string]any{"capabilities": []any{capability}}})
		case "invoke":
			var params struct {
				Input struct {
					Block bool `json:"block"`
				} `json:"input"`
			}
			_ = json.Unmarshal(req.Params, &params)
			_ = encoder.Encode(map[string]any{"version": version, "id": req.ID, "event": map[string]any{"sequence": 1, "type": "progress", "data": map[string]any{"step": 1}}})
			if params.Input.Block {
				held = req.ID
				continue
			}
			_ = encoder.Encode(map[string]any{"version": version, "id": req.ID, "artifact": map[string]any{"sequence": 2, "name": "report.txt", "media_type": "text/plain", "encoding": "base64", "data": base64.StdEncoding.EncodeToString([]byte("artifact"))}})
			_ = encoder.Encode(map[string]any{"version": version, "id": req.ID, "result": map[string]any{"output": map[string]any{"ok": true}}})
		case "cancel":
			var params struct {
				InvocationID string `json:"invocation_id"`
			}
			_ = json.Unmarshal(req.Params, &params)
			if held == "" || held != params.InvocationID {
				_ = encoder.Encode(map[string]any{"version": version, "id": req.ID, "error": map[string]any{"code": "not_cancelable", "message": "invocation is not active"}})
				continue
			}
			_ = encoder.Encode(map[string]any{"version": version, "id": req.ID, "result": map[string]any{"canceled": true}})
			_ = encoder.Encode(map[string]any{"version": version, "id": held, "error": map[string]any{"code": "canceled", "message": "invocation canceled"}})
			held = ""
		case "health":
			_ = encoder.Encode(map[string]any{"version": version, "id": req.ID, "result": map[string]any{"healthy": true, "message": "ok"}})
		}
	}
}
