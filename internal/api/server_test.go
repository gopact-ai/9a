package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"github.com/gopact-ai/9a/internal/app"
	callmodel "github.com/gopact-ai/9a/internal/call"
	"github.com/gopact-ai/9a/internal/store"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCallQuotaErrorIsStable(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "ninea.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := callmodel.NewRepository(db)
	for i := range callmodel.MaxIdentityActiveCalls {
		call := callmodel.Call{
			ID:           fmt.Sprintf("call-api-quota-%d", i),
			CapabilityID: "echo/demo/echo",
			IdentityID:   "owner",
			State:        callmodel.Submitted,
		}
		if err := repo.Create(ctx, call, json.RawMessage(`{}`)); err != nil {
			t.Fatal(err)
		}
	}
	quotaErr := repo.Create(ctx, callmodel.Call{
		ID:           "call-api-quota-over",
		CapabilityID: "echo/demo/echo",
		IdentityID:   "owner",
		State:        callmodel.Submitted,
	}, json.RawMessage(`{}`))

	recorder := httptest.NewRecorder()
	writeCallError(recorder, quotaErr)
	var response Response
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	if recorder.Code != http.StatusBadRequest || response.Code != "call_quota_exceeded" || response.Error != "call quota exceeded" {
		t.Fatalf("status=%d response=%#v", recorder.Code, response)
	}
}

func testSocket(t *testing.T) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "ninea-api-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	_ = f.Close()
	_ = os.Remove(path)
	t.Cleanup(func() { _ = os.Remove(path) })
	return path
}

func TestNonAdminTokenCannotUseAdministrativeActions(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := app.New(db)
	if err := a.Bootstrap(ctx, "root-secret"); err != nil {
		t.Fatal(err)
	}
	agent, err := a.CreateToken(ctx, "agent")
	if err != nil {
		t.Fatal(err)
	}
	socket := testSocket(t)
	s, err := Listen(socket, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close(context.Background()) })
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	}}
	requests := []Request{{Action: "acl.grant", Identity: "agent", Capability: "x", Permissions: []string{"invoke"}}, {Action: "adapter.add", Protocol: "evil", Executable: "/bin/true"}, {Action: "provider.add", Protocol: "mcp", Name: "evil", Endpoint: "stdio:/bin/true"}, {Action: "token.create", Identity: "attacker"}}
	for _, bodyRequest := range requests {
		bodyRequest := bodyRequest
		t.Run(bodyRequest.Action, func(t *testing.T) {
			body, _ := json.Marshal(bodyRequest)
			req, _ := http.NewRequest("POST", "http://unix/rpc", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer "+agent)
			resp, err := (&http.Client{Transport: tr}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			var out Response
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusForbidden || out.Code != "permission_denied" {
				t.Fatalf("status=%d response=%#v", resp.StatusCode, out)
			}
		})
	}
}

func TestAdminCanAddAdapterAndReceivesSanitizedStableErrors(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := app.New(db)
	if err := a.Bootstrap(ctx, "root-secret"); err != nil {
		t.Fatal(err)
	}
	socket := testSocket(t)
	s, err := Listen(socket, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	}}
	client := &http.Client{Transport: transport}
	call := func(request Request) (int, Response) {
		t.Helper()
		body, _ := json.Marshal(request)
		req, _ := http.NewRequest("POST", "http://unix/rpc", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer root-secret")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	executable := filepath.Join(t.TempDir(), "billing-adapter")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	status, out := call(Request{Action: "adapter.add", Protocol: "billing", Executable: executable})
	if status != http.StatusOK || out.Error != "" {
		t.Fatalf("first add status=%d response=%#v", status, out)
	}
	status, out = call(Request{Action: "adapter.add", Protocol: "billing", Executable: executable})
	if status != http.StatusBadRequest || out.Code != "adapter_exists" || out.Error != "adapter already registered" {
		t.Fatalf("duplicate status=%d response=%#v", status, out)
	}
	secretPath := filepath.Join(t.TempDir(), "secret-adapter-token")
	status, out = call(Request{Action: "adapter.add", Protocol: "secret", Executable: secretPath})
	if status != http.StatusBadRequest || out.Code != "invalid_adapter" || out.Error != "invalid adapter registration" {
		t.Fatalf("invalid status=%d response=%#v", status, out)
	}
	if strings.Contains(out.Error, secretPath) || strings.Contains(out.Error, "secret-adapter-token") {
		t.Fatalf("invalid response leaked executable: %#v", out)
	}
	sensitive := "sensitive-adapter-insert-detail-7f4c"
	if _, err := db.Exec(`CREATE TRIGGER reject_sensitive_adapter BEFORE INSERT ON external_adapters WHEN NEW.protocol='broken' BEGIN SELECT RAISE(FAIL,'` + sensitive + `'); END`); err != nil {
		t.Fatal(err)
	}
	status, out = call(Request{Action: "adapter.add", Protocol: "broken", Executable: executable})
	if status != http.StatusBadRequest || out.Code != "adapter_failed" || out.Error != "adapter registration failed" {
		t.Fatalf("repository failure status=%d response=%#v", status, out)
	}
	encoded, _ := json.Marshal(out)
	if bytes.Contains(encoded, []byte(sensitive)) || bytes.Contains(encoded, []byte("external_adapters")) || bytes.Contains(encoded, []byte("constraint")) {
		t.Fatalf("repository failure leaked internal details: %s", encoded)
	}
}

func TestListenRefusesRegularFileAndLiveSocket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	a := app.New(db)
	if err := a.Bootstrap(ctx, "root"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "socket")
	if err := os.WriteFile(path, []byte("user"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Listen(path, a); err == nil {
		t.Fatal("regular file replaced")
	}
	if data, _ := os.ReadFile(path); string(data) != "user" {
		t.Fatal("regular file changed")
	}
	path = testSocket(t)
	s, err := Listen(path, a)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	if _, err := Listen(path, a); err == nil {
		t.Fatal("live socket unlinked")
	}
}

func TestCallActionsRouteAndHideMissingCalls(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	a := app.New(db)
	if err := a.Bootstrap(ctx, "root-secret"); err != nil {
		t.Fatal(err)
	}
	ownerToken, err := a.CreateToken(ctx, "owner")
	if err != nil {
		t.Fatal(err)
	}
	socket := testSocket(t)
	s, err := Listen(socket, a)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	}}
	callRPC := func(request Request) (int, Response) {
		t.Helper()
		body, _ := json.Marshal(request)
		req, _ := http.NewRequest("POST", "http://unix/rpc", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out Response
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, out
	}
	for _, action := range []string{"call.get", "call.events", "call.cancel"} {
		status, out := callRPC(Request{Action: action, CallID: "call-does-not-exist"})
		if status != http.StatusBadRequest || out.Code != "call_not_found" || out.Error != "call not found" {
			t.Fatalf("%s status=%d response=%#v", action, status, out)
		}
	}
	status, out := callRPC(Request{Action: "call.start", Capability: "missing/capability", Input: json.RawMessage(`{}`)})
	if status != http.StatusBadRequest || out.Code != "request_failed" || out.Error != "call request failed" {
		t.Fatalf("call.start status=%d response=%#v", status, out)
	}
	repo := callmodel.NewRepository(db)
	if err := repo.Create(ctx, callmodel.Call{ID: "call-page-api", CapabilityID: "test/capability", IdentityID: "owner", State: callmodel.Completed}, json.RawMessage(`{}`)); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 3; i++ {
		if _, err := repo.AppendEvent(ctx, "call-page-api", json.RawMessage(`{"kind":"event"}`)); err != nil {
			t.Fatal(err)
		}
	}
	status, out = callRPC(Request{Action: "call.events", CallID: "call-page-api", Limit: 2})
	encoded, _ := json.Marshal(out.Data)
	var first callmodel.EventPage
	if err := json.Unmarshal(encoded, &first); err != nil || status != http.StatusOK || len(first.Events) != 2 || !first.HasMore || first.NextAfter != 2 {
		t.Fatalf("first event page status=%d page=%#v err=%v", status, first, err)
	}
	status, out = callRPC(Request{Action: "call.events", CallID: "call-page-api", After: first.NextAfter, Limit: 2})
	encoded, _ = json.Marshal(out.Data)
	var second callmodel.EventPage
	if err := json.Unmarshal(encoded, &second); err != nil || status != http.StatusOK || len(second.Events) != 1 || second.HasMore || second.NextAfter != 3 {
		t.Fatalf("second event page status=%d page=%#v err=%v", status, second, err)
	}
	if _, err := db.Exec(`DROP TABLE calls`); err != nil {
		t.Fatal(err)
	}
	status, out = callRPC(Request{Action: "call.get", CallID: "call-does-not-exist"})
	if status != http.StatusBadRequest || out.Code != "request_failed" || out.Error != "call request failed" {
		t.Fatalf("call.get repository failure status=%d response=%#v", status, out)
	}
}
