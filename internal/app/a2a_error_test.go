package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/call"
)

func TestA2ASyncAndAsyncErrorsDoNotExposeUpstreamDetails(t *testing.T) {
	const sentinel = "https://internal.example token=secret tenant=private dial tcp 10.0.0.1"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/.well-known/agent-card.json" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"name": "Safe Agent", "description": "Returns safe failures.", "version": "1.0.0",
				"supportedInterfaces": []any{map[string]any{"url": server.URL + "/a2a/v1", "protocolBinding": "HTTP+JSON", "protocolVersion": "1.0"}},
				"capabilities":        map[string]any{}, "defaultInputModes": []string{"text/plain"}, "defaultOutputModes": []string{"text/plain"},
				"skills": []any{map[string]any{"id": "fail", "name": "Fail", "description": "Return a failure.", "tags": []string{"failure"}}},
			})
			return
		}
		w.Header().Set("Content-Type", "application/a2a+json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(sentinel))
	}))
	defer server.Close()

	a, _ := testApp(t)
	defer func() { _ = a.Close(context.Background()) }()
	ctx := context.Background()
	if err := a.Bootstrap(ctx, "secret"); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cleanupReadOnlyProjection(t, root)
	source := []byte(fmt.Sprintf("version: 1\nname: safe-agent\ntype: a2a\nurl: %s\n", server.URL))
	if _, err := a.Connect(ctx, "admin", source, root); err != nil {
		t.Fatal(err)
	}
	input := json.RawMessage(`{"parts":[{"text":"fail"}]}`)
	approval := approvalForRun(t, a, "admin", root, "safe-agent/fail", input)
	_, err := a.RunInWorkspace(ctx, "admin", root, "safe-agent/fail", input, approval)
	if err == nil {
		t.Fatal("RunInWorkspace unexpectedly succeeded")
	}
	assertNoA2ASecret(t, err.Error(), sentinel)
	var runErr *RunError
	if !errors.As(err, &runErr) || runErr.CallID == "" {
		t.Fatalf("RunInWorkspace error=%#v", err)
	}
	var record call.Record
	for deadline := time.Now().Add(time.Second); ; {
		record, err = a.getCall(ctx, "admin", runErr.CallID)
		if err == nil && record.Call.State == call.Failed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("call did not fail safely: %#v error=%v", record, err)
		}
		time.Sleep(time.Millisecond)
	}
	assertNoA2ASecret(t, record.Call.Code+" "+record.Call.Message, sentinel)
}

func assertNoA2ASecret(t *testing.T, value, sentinel string) {
	t.Helper()
	for _, fragment := range []string{"internal.example", "token=secret", "tenant=private", "10.0.0.1", sentinel} {
		if strings.Contains(value, fragment) {
			t.Fatalf("value leaked upstream detail: %q", value)
		}
	}
}
