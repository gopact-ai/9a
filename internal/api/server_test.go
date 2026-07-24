package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gopact-ai/9a/internal/app"
	"github.com/gopact-ai/9a/internal/store"
)

func TestDecodeRequestIsStrict(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want Request
		err  bool
	}{
		{name: "valid", body: `{"action":"status","name":"weather"}`, want: Request{Action: "status", Name: "weather"}},
		{name: "unknown field", body: `{"action":"status","typo":true}`, err: true},
		{name: "second value", body: `{"action":"status"} {"action":"doctor"}`, err: true},
		{name: "missing action", body: `{"name":"weather"}`, err: true},
		{name: "null", body: `null`, err: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, err := decodeRequest(strings.NewReader(test.body))
			if (err != nil) != test.err {
				t.Fatalf("decode error=%v want error=%v", err, test.err)
			}
			if !test.err && !reflect.DeepEqual(got, test.want) {
				t.Fatalf("request=%#v want %#v", got, test.want)
			}
		})
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

func TestUnknownTokenCannotUseRuntimeActions(t *testing.T) {
	t.Parallel()
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
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "unix", socket)
	}}
	requests := []Request{
		{Action: "connect", Source: "invalid", Root: t.TempDir()},
		{Action: "disconnect", Name: "weather"},
		{Action: "secret.set", Name: "weather.token", Root: t.TempDir(), Value: "hidden"},
		{Action: "secret.unset", Name: "weather.token", Root: t.TempDir()},
	}
	for _, bodyRequest := range requests {
		t.Run(bodyRequest.Action, func(t *testing.T) {
			body, _ := json.Marshal(bodyRequest)
			req, _ := http.NewRequest(http.MethodPost, "http://unix/rpc", bytes.NewReader(body))
			req.Header.Set("Authorization", "Bearer unknown-token")
			resp, err := (&http.Client{Transport: tr}).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			var out Response
			if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
				_ = resp.Body.Close()
				t.Fatal(err)
			}
			if err := resp.Body.Close(); err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != http.StatusUnauthorized || out.Code != "unauthorized" {
				t.Fatalf("status=%d response=%#v", resp.StatusCode, out)
			}
		})
	}
}

func TestRemovedRPCSurfacesAreUnknownActions(t *testing.T) {
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
	for _, action := range []string{"adapter.add", "provider.add", "provider.remove", "workspace.attach", "workspace.update", "workspace.detach", "acl.grant", "token.create", "project.add", "call.start", "call.get", "call.events", "call.cancel", "invoke", "declarative.add", "declarative.diff", "declarative.remove"} {
		body, _ := json.Marshal(Request{Action: action})
		req, _ := http.NewRequest(http.MethodPost, "http://unix/rpc", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer root-secret")
		resp, err := (&http.Client{Transport: transport}).Do(req)
		if err != nil {
			t.Fatal(err)
		}
		var out Response
		decodeErr := json.NewDecoder(resp.Body).Decode(&out)
		_ = resp.Body.Close()
		if decodeErr != nil || resp.StatusCode != http.StatusBadRequest || out.Error != "unknown_action" {
			t.Fatalf("%s status=%d response=%#v decode=%v", action, resp.StatusCode, out, decodeErr)
		}
	}
}

func TestWriteRunErrorIncludesPersistentCallIdentity(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeRunError(recorder, &app.RunError{CallID: "call-123", Code: "upstream_failed", Message: "upstream unavailable"})
	var response Response
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	data, ok := response.Data.(map[string]any)
	if recorder.Code != http.StatusBadRequest || response.Code != "upstream_failed" || !ok || data["call_id"] != "call-123" {
		t.Fatalf("status=%d response=%#v", recorder.Code, response)
	}
}

func TestWriteRunErrorExplainsApprovalWithoutSideEffect(t *testing.T) {
	recorder := httptest.NewRecorder()
	const token = "v1.7.0123456789abcdef0123456789abcdef"
	writeRunError(recorder, &app.ApprovalRequiredError{Capability: "weather/update", Token: token})
	var response Response
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	data, ok := response.Data.(map[string]any)
	next, nextOK := data["nextAction"].(map[string]any)
	if recorder.Code != http.StatusConflict || response.Code != "approval_required" || !ok || data["approvalToken"] != token || data["sideEffect"] != "none" || !nextOK || next["instruction"] != "Obtain explicit approval, then retry the exact same input with --approve "+token {
		t.Fatalf("status=%d response=%#v", recorder.Code, response)
	}
}

func TestWriteRunErrorRejectsApprovalForDifferentInput(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeRunError(recorder, &app.ApprovalMismatchError{Capability: "weather/update"})
	var response Response
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	data, ok := response.Data.(map[string]any)
	if recorder.Code != http.StatusConflict || response.Code != "approval_mismatch" || !ok || data["sideEffect"] != "none" {
		t.Fatalf("status=%d response=%#v", recorder.Code, response)
	}
}

func TestWriteRunErrorRequiresFreshReviewWhenCapabilityChanged(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeRunError(recorder, &app.CapabilityChangedError{Capability: "weather/update"})
	var response Response
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	data, ok := response.Data.(map[string]any)
	next, nextOK := data["nextAction"].(map[string]any)
	if recorder.Code != http.StatusConflict || response.Code != "capability_changed" || !ok || data["sideEffect"] != "none" || !nextOK || next["command"] != "9a search weather/update --json" {
		t.Fatalf("response=%#v data=%#v", response, data)
	}
}

func TestWriteRunErrorExplainsMissingCredentialWithoutValue(t *testing.T) {
	recorder := httptest.NewRecorder()
	writeRunError(recorder, &app.RunError{
		CallID:     "call-credential",
		Code:       "missing_credential",
		Message:    "credential weather.api-token is missing",
		Credential: "weather.api-token",
		SideEffect: "none",
	})
	var response Response
	if err := json.NewDecoder(recorder.Body).Decode(&response); err != nil {
		t.Fatal(err)
	}
	data, ok := response.Data.(map[string]any)
	next, nextOK := data["nextAction"].(map[string]any)
	if recorder.Code != http.StatusConflict || response.Code != "missing_credential" || !ok || data["sideEffect"] != "none" || data["call_id"] != "call-credential" || !nextOK || next["command"] != "9a secret set weather.api-token" {
		t.Fatalf("status=%d response=%#v", recorder.Code, response)
	}
}

func TestListenRefusesRegularFileAndLiveSocket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
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
	defer func() { _ = s.Close(context.Background()) }()
	if _, err := Listen(path, a); err == nil {
		t.Fatal("live socket unlinked")
	}
}
