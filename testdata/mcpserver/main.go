package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
)

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

func main() {
	s := bufio.NewScanner(os.Stdin)
	e := json.NewEncoder(os.Stdout)
	for s.Scan() {
		var r request
		if json.Unmarshal(s.Bytes(), &r) != nil {
			continue
		}
		if r.ID == 0 {
			continue
		}
		var result any
		switch r.Method {
		case "initialize":
			if os.Getenv("NINEA_TOKEN") != "" || os.Getenv("NINEA_BOOTSTRAP_TOKEN") != "" {
				_ = e.Encode(map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32000, "message": "daemon credential leaked to provider"}})
				continue
			}
			result = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
		case "tools/list":
			if os.Getenv("MCP_FIXTURE_MODE") == "malformed" {
				_ = e.Encode(map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": map[string]any{}})
				continue
			}
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(r.Params, &params)
			if params.Cursor == "page2" {
				result = map[string]any{"tools": []any{map[string]any{"name": "get_forecast", "description": "Get weather forecast", "inputSchema": map[string]any{"type": "object"}}}}
			} else {
				result = map[string]any{"tools": []any{map[string]any{"name": "get_weather", "description": "Get current weather temperature " + strings.Repeat("x", 70_000), "inputSchema": map[string]any{"type": "object"}}}, "nextCursor": "page2"}
			}
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(r.Params, &params)
			if params.Name != "get_weather" || params.Arguments["location"] != "Shanghai" {
				_ = e.Encode(map[string]any{"jsonrpc": "2.0", "id": r.ID, "error": map[string]any{"code": -32602, "message": "unexpected tool or arguments"}})
				continue
			}
			if path := os.Getenv("NINEA_FIXTURE_COUNTER"); path != "" {
				f, _ := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				if f != nil {
					_, _ = f.WriteString("call\n")
					_ = f.Close()
				}
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "{\"temperature\":26,\"unit\":\"C\"}"}}, "isError": false}
		default:
			result = map[string]any{}
		}
		_ = e.Encode(map[string]any{"jsonrpc": "2.0", "id": r.ID, "result": result})
	}
}
