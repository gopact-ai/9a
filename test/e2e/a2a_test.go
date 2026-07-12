package e2e

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestA2ADiscoveryRestoreSyncAsyncCancelAndCLI(t *testing.T) {
	if testing.Short() {
		t.Skip("process e2e")
	}
	var mu sync.Mutex
	discoveries := 0
	completedPolls := 0
	cancelPolls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() == "/.well-known/agent-card.json" {
			if r.Header.Get("Authorization") != "" {
				t.Error("discovery leaked operation credentials")
			}
			mu.Lock()
			discoveries++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "Research Agent", "description": "Summarizes supplied material.", "version": "1.0.0",
				"capabilities":         map[string]any{"streaming": false},
				"securitySchemes":      map[string]any{"bearerAuth": map[string]any{"httpAuthSecurityScheme": map[string]any{"scheme": "Bearer"}}},
				"securityRequirements": []any{map[string]any{"schemes": map[string]any{"bearerAuth": map[string]any{"list": []any{}}}}},
				"supportedInterfaces":  []any{map[string]any{"url": "http://" + r.Host + "/a2a/v1", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0", "tenant": "tenant-e2e"}},
				"defaultInputModes":    []string{"text/plain"}, "defaultOutputModes": []string{"text/plain"},
				"skills": []any{map[string]any{"id": "summarize", "name": "Summarize", "description": "Summarize supplied material.", "tags": []string{"summary"}}},
			})
			return
		}
		if r.Header.Get("Authorization") != "Bearer a2a-e2e-secret" {
			t.Errorf("operation Authorization=%q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("A2A-Version") != "1.0" {
			t.Errorf("A2A-Version=%q", r.Header.Get("A2A-Version"))
		}
		w.Header().Set("Content-Type", "application/a2a+json")
		switch r.URL.EscapedPath() {
		case "/a2a/v1/message:send":
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			configuration, _ := request["configuration"].(map[string]any)
			message, _ := request["message"].(map[string]any)
			parts, _ := message["parts"].([]any)
			part, _ := parts[0].(map[string]any)
			mode, _ := part["text"].(string)
			if request["tenant"] != "tenant-e2e" || configuration["returnImmediately"] != true {
				t.Errorf("send request=%#v", request)
			}
			switch mode {
			case "direct":
				_ = json.NewEncoder(w).Encode(map[string]any{"message": map[string]any{"messageId": "a2a-response", "contextId": "direct-context", "role": "ROLE_AGENT", "parts": []any{map[string]any{"text": "summary ready"}}}})
			case "async":
				_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{
					"id": "completed-task", "contextId": "async-context", "status": map[string]any{"state": "TASK_STATE_SUBMITTED"},
					"artifacts": []any{map[string]any{"artifactId": "draft", "name": "Draft", "parts": []any{map[string]any{"data": []any{"draft"}}}}},
				}})
			case "cancel":
				_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{"id": "cancel-task", "contextId": "cancel-context", "status": map[string]any{"state": "TASK_STATE_SUBMITTED"}}})
			default:
				http.Error(w, "unknown mode", http.StatusBadRequest)
			}
		case "/a2a/v1/tasks/completed-task":
			if r.URL.Query().Get("tenant") != "tenant-e2e" {
				t.Errorf("poll tenant=%q", r.URL.Query().Get("tenant"))
			}
			mu.Lock()
			completedPolls++
			poll := completedPolls
			mu.Unlock()
			state := "TASK_STATE_WORKING"
			artifacts := []any{map[string]any{"artifactId": "draft", "name": "Draft", "parts": []any{map[string]any{"data": []any{"draft"}}}}}
			if poll >= 2 {
				state = "TASK_STATE_COMPLETED"
				artifacts = append(artifacts, map[string]any{"artifactId": "final", "name": "Final", "parts": []any{map[string]any{"text": "final summary"}}})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "completed-task", "contextId": "async-context", "status": map[string]any{"state": state}, "artifacts": artifacts})
		case "/a2a/v1/tasks/cancel-task":
			if r.URL.Query().Get("tenant") != "tenant-e2e" {
				t.Errorf("cancel poll tenant=%q", r.URL.Query().Get("tenant"))
			}
			mu.Lock()
			cancelPolls++
			mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "cancel-task", "contextId": "cancel-context", "status": map[string]any{"state": "TASK_STATE_WORKING"}})
		case "/a2a/v1/tasks/cancel-task:cancel":
			var request map[string]any
			_ = json.NewDecoder(r.Body).Decode(&request)
			if request["tenant"] != "tenant-e2e" {
				t.Errorf("cancel request=%#v", request)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "cancel-task", "contextId": "cancel-context", "status": map[string]any{"state": "TASK_STATE_CANCELED"}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	root := t.TempDir()
	cli, daemon := filepath.Join(root, "9a"), filepath.Join(root, "ninead")
	build(t, cli, "./cmd/9a")
	build(t, daemon, "./cmd/ninead")
	socket := socketPath(t)
	state := filepath.Join(root, "state.db")
	token := "a2a-admin-secret"
	adminEnv := append(os.Environ(), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+token, "NINEA_A2A_TOKEN_RESEARCH_AGENT=a2a-e2e-secret")
	var logs bytes.Buffer
	startDaemon := func(bootstrap bool) *exec.Cmd {
		d := exec.Command(daemon, "--state", state, "--socket", socket)
		d.Env = adminEnv
		if bootstrap {
			d.Env = append(d.Env, "NINEA_BOOTSTRAP_TOKEN="+token)
		}
		d.Stderr = &logs
		if err := d.Start(); err != nil {
			t.Fatal(err)
		}
		waitSocket(t, socket, &logs)
		return d
	}
	d := startDaemon(true)
	t.Cleanup(func() { _ = d.Process.Kill(); _ = d.Wait() })
	run(t, adminEnv, cli, "", "providers", "add", "a2a", "research-agent", server.URL)
	agentToken := strings.TrimSpace(string(run(t, adminEnv, cli, "", "tokens", "create", "agent")))
	agentEnv := append(os.Environ(), "NINEA_SOCKET="+socket, "NINEA_TOKEN="+agentToken)
	run(t, adminEnv, cli, "", "acl", "grant", "agent", "a2a/research-agent/summarize", "read,invoke")
	search := run(t, agentEnv, cli, "", "search", "summarize", "--format", "json")
	var results []map[string]any
	if err := json.Unmarshal(search, &results); err != nil || len(results) != 1 {
		t.Fatalf("search=%s err=%v", search, err)
	}
	skills := filepath.Join(root, "skills")
	run(t, agentEnv, cli, "", "project", "add", "a2a/research-agent/summarize", skills)
	if data, err := os.ReadFile(filepath.Join(skills, "ninea-a2a-research-agent-summarize", "SKILL.md")); err != nil || !bytes.Contains(data, []byte("Summarize supplied material")) {
		t.Fatalf("projected SKILL.md=%s err=%v", data, err)
	}
	if err := d.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = d.Wait()
	_ = os.Remove(socket)
	d = startDaemon(false)
	out := run(t, agentEnv, cli, `{"parts":[{"text":"direct"}]}`, "invoke", "a2a/research-agent/summarize")
	var message map[string]any
	if err := json.Unmarshal(out, &message); err != nil || message["messageId"] != "a2a-response" || message["contextId"] != "direct-context" {
		t.Fatalf("invoke=%s err=%v", out, err)
	}
	completedID := strings.TrimSpace(string(run(t, agentEnv, cli, `{"parts":[{"text":"async"}]}`, "calls", "start", "a2a/research-agent/summarize")))
	completed := waitCall(t, agentEnv, cli, completedID, "completed")
	if !bytes.Contains(completed.Result, []byte(`"TASK_STATE_COMPLETED"`)) {
		t.Fatalf("completed=%#v", completed)
	}
	type eventPage struct {
		Events []struct {
			Sequence int             `json:"sequence"`
			Envelope json.RawMessage `json:"envelope"`
		} `json:"events"`
		NextAfter int  `json:"next_after"`
		HasMore   bool `json:"has_more"`
	}
	var envelopes []json.RawMessage
	after := 0
	for {
		args := []string{"calls", "events", completedID, "--limit", "2"}
		if after != 0 {
			args = append(args, "--after", strconv.Itoa(after))
		}
		pageData := run(t, agentEnv, cli, "", args...)
		var page eventPage
		if err := json.Unmarshal(pageData, &page); err != nil || len(page.Events) == 0 {
			t.Fatalf("events=%s err=%v", pageData, err)
		}
		for _, event := range page.Events {
			envelopes = append(envelopes, event.Envelope)
		}
		if !page.HasMore {
			break
		}
		after = page.NextAfter
	}
	wantFragments := []string{`"type":"status"`, `"kind":"artifact"`, `"type":"status"`, `"type":"status"`, `"kind":"artifact"`, `"type":"result"`}
	if len(envelopes) != len(wantFragments) {
		t.Fatalf("event envelopes=%s", envelopes)
	}
	for i, fragment := range wantFragments {
		if !bytes.Contains(envelopes[i], []byte(fragment)) {
			t.Fatalf("event %d=%s want %s", i+1, envelopes[i], fragment)
		}
	}
	cancelID := strings.TrimSpace(string(run(t, agentEnv, cli, `{"parts":[{"text":"cancel"}]}`, "calls", "start", "a2a/research-agent/summarize")))
	_ = waitCall(t, agentEnv, cli, cancelID, "working")
	for deadline := time.Now().Add(5 * time.Second); ; {
		mu.Lock()
		polled := cancelPolls > 0
		mu.Unlock()
		if polled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("cancelable A2A task was not polled")
		}
		time.Sleep(10 * time.Millisecond)
	}
	run(t, agentEnv, cli, "", "calls", "cancel", cancelID)
	canceled := waitCall(t, agentEnv, cli, cancelID, "canceled")
	if canceled.Call.Code != "canceled" {
		t.Fatalf("canceled=%#v", canceled)
	}
	mu.Lock()
	defer mu.Unlock()
	if discoveries != 2 || completedPolls != 2 || cancelPolls == 0 {
		t.Fatalf("discoveries=%d completedPolls=%d cancelPolls=%d", discoveries, completedPolls, cancelPolls)
	}
}
