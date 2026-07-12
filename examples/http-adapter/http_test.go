package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func testOperation(name, method, path, auth string) operation {
	return operation{
		UpstreamName: name, Name: name, Description: "Test operation.", Method: method, Path: path,
		InputSchema: map[string]any{"type": "object"}, OutputSchema: map[string]any{"type": "object"},
		Auth: auth, RequiresApproval: "never",
	}
}

func testManifest(t *testing.T, operations ...operation) *manifest {
	t.Helper()
	m := &manifest{Version: "1", Operations: operations}
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestHTTPBridgeMethodsQueryBodyAndAuthIsolation(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]struct {
		query, body, auth, contentType string
	}{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		seen[r.Method+" "+r.URL.Path] = struct{ query, body, auth, contentType string }{r.URL.RawQuery, string(body), r.Header.Get("Authorization"), r.Header.Get("Content-Type")}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer server.Close()

	m := testManifest(t,
		testOperation("read", "GET", "/read", "bearer"),
		testOperation("remove", "DELETE", "/remove", "none"),
		testOperation("create", "POST", "/create", "none"),
		testOperation("replace", "PUT", "/replace", "none"),
		testOperation("update", "PATCH", "/update", "none"),
	)
	bridge := newHTTPBridge(m)
	t.Setenv("NINEA_HTTP_TOKEN_PRIVATE_API", "private-secret")
	t.Setenv("NINEA_HTTP_TOKEN_PUBLIC_API", "must-not-leak")
	private := providerConfig{Name: "private-api", Endpoint: server.URL}
	public := providerConfig{Name: "public-api", Endpoint: server.URL}
	if _, fault := bridge.invoke(context.Background(), private, "read", json.RawMessage(`{"z":true,"a":"x y","n":2,"empty":null}`)); fault != nil {
		t.Fatal(fault)
	}
	if _, fault := bridge.invoke(context.Background(), public, "remove", json.RawMessage(`{"id":"123"}`)); fault != nil {
		t.Fatal(fault)
	}
	for _, method := range []string{"POST", "PUT", "PATCH"} {
		name := map[string]string{"POST": "create", "PUT": "replace", "PATCH": "update"}[method]
		if _, fault := bridge.invoke(context.Background(), public, name, json.RawMessage(`{"id":"123"}`)); fault != nil {
			t.Fatal(fault)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if got := seen["GET /read"]; got.query != "a=x+y&empty=&n=2&z=true" || got.body != "" || got.auth != "Bearer private-secret" || got.contentType != "" {
		t.Fatalf("GET=%#v", got)
	}
	if got := seen["DELETE /remove"]; got.query != "id=123" || got.auth != "" {
		t.Fatalf("DELETE=%#v", got)
	}
	for _, method := range []string{"POST", "PUT", "PATCH"} {
		path := map[string]string{"POST": "/create", "PUT": "/replace", "PATCH": "/update"}[method]
		got := seen[method+" "+path]
		if got.body != `{"id":"123"}` || got.auth != "" || got.contentType != "application/json" || got.query != "" {
			t.Fatalf("%s=%#v", method, got)
		}
	}
}

func TestHTTPBridgeRejectsUnsafeProviderAndInput(t *testing.T) {
	bridge := newHTTPBridge(testManifest(t, testOperation("read", "GET", "/read", "none")))
	for _, test := range []struct {
		name     string
		provider providerConfig
		input    string
	}{
		{name: "noncanonical provider", provider: providerConfig{Name: "Private_API", Endpoint: "https://api.example"}, input: `{}`},
		{name: "cleartext remote", provider: providerConfig{Name: "private-api", Endpoint: "http://api.example"}, input: `{}`},
		{name: "endpoint credentials", provider: providerConfig{Name: "private-api", Endpoint: "https://user:pass@api.example"}, input: `{}`},
		{name: "endpoint query", provider: providerConfig{Name: "private-api", Endpoint: "https://api.example?secret=x"}, input: `{}`},
		{name: "GET array", provider: providerConfig{Name: "private-api", Endpoint: "https://api.example"}, input: `[]`},
		{name: "GET nested", provider: providerConfig{Name: "private-api", Endpoint: "https://api.example"}, input: `{"nested":{"x":1}}`},
		{name: "malformed JSON", provider: providerConfig{Name: "private-api", Endpoint: "https://api.example"}, input: `{`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, fault := bridge.invoke(context.Background(), test.provider, "read", json.RawMessage(test.input)); fault == nil || fault.Code != "invalid_request" {
				t.Fatalf("fault=%#v", fault)
			}
		})
	}
}

func TestHTTPBridgeRedirectResponseLimitsTimeoutAndSafeErrors(t *testing.T) {
	secret := "private-secret"
	t.Setenv("NINEA_HTTP_TOKEN_PRIVATE_API", secret)
	var externalAuth string
	external := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		externalAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer external.Close()
	for _, test := range []struct {
		name    string
		handler http.HandlerFunc
		code    string
		timeout time.Duration
	}{
		{name: "cross origin redirect", code: "upstream_unavailable", handler: func(w http.ResponseWriter, _ *http.Request) {
			http.Redirect(w, &http.Request{}, external.URL, http.StatusFound)
		}},
		{name: "bad status", code: "upstream_error", handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "token="+secret, http.StatusBadGateway) }},
		{name: "wrong content type", code: "invalid_response", handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{}`) }},
		{name: "malformed response", code: "invalid_response", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{`)
		}},
		{name: "oversized response", code: "response_too_large", handler: func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `"`+strings.Repeat("x", maxHTTPBodyBytes)+`"`)
		}},
		{name: "timeout", code: "upstream_unavailable", timeout: 10 * time.Millisecond, handler: func(w http.ResponseWriter, _ *http.Request) {
			time.Sleep(50 * time.Millisecond)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{}`)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()
			bridge := newHTTPBridge(testManifest(t, testOperation("read", "POST", "/read", "bearer")))
			if test.timeout != 0 {
				bridge.client.Timeout = test.timeout
			}
			_, fault := bridge.invoke(context.Background(), providerConfig{Name: "private-api", Endpoint: server.URL}, "read", json.RawMessage(`{"secret":"body-secret"}`))
			if fault == nil || fault.Code != test.code {
				t.Fatalf("fault=%#v", fault)
			}
			message := fault.Error()
			for _, forbidden := range []string{server.URL, external.URL, secret, "body-secret"} {
				if strings.Contains(message, forbidden) {
					t.Fatalf("unsafe error %q contains %q", message, forbidden)
				}
			}
		})
	}
	if externalAuth != "" {
		t.Fatalf("redirect leaked Authorization=%q", externalAuth)
	}
}

func TestHTTPBridgeRequiresBearerTokenAndReturnsJSON(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json; charset=utf-8")
		_, _ = io.WriteString(w, `{"value":42}`)
	}))
	defer server.Close()
	bridge := newHTTPBridge(testManifest(t, testOperation("read", "POST", "/read", "bearer")))
	p := providerConfig{Name: "private-api", Endpoint: server.URL}
	if _, fault := bridge.invoke(context.Background(), p, "read", json.RawMessage(`{}`)); fault == nil || fault.Code != "missing_credentials" {
		t.Fatalf("fault=%#v", fault)
	}
	t.Setenv("NINEA_HTTP_TOKEN_PRIVATE_API", "secret")
	output, fault := bridge.invoke(context.Background(), p, "read", json.RawMessage(`{}`))
	if fault != nil || string(output) != `{"value":42}` {
		t.Fatalf("output=%s fault=%v", output, fault)
	}
}

func TestHTTPBridgeHealthUsesIndependentAuthPolicy(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()
	m := testManifest(t, testOperation("public", "POST", "/public", "none"))
	m.HealthPath, m.HealthAuth = "/healthz", "bearer"
	bridge := newHTTPBridge(m)
	provider := providerConfig{Name: "health-api", Endpoint: server.URL}
	if fault := bridge.health(context.Background(), provider); fault == nil || fault.Code != "missing_credentials" {
		t.Fatalf("fault=%#v", fault)
	}
	t.Setenv("NINEA_HTTP_TOKEN_HEALTH_API", "health-secret")
	if fault := bridge.health(context.Background(), provider); fault != nil {
		t.Fatal(fault)
	}
	if authorization != "Bearer health-secret" {
		t.Fatalf("Authorization=%q", authorization)
	}
	m.HealthAuth = "none"
	if fault := bridge.health(context.Background(), provider); fault != nil {
		t.Fatal(fault)
	}
	if authorization != "" {
		t.Fatalf("public health Authorization=%q", authorization)
	}
}

func TestAdapterFaultNeverWrapsInternalError(t *testing.T) {
	fault := safeFault("upstream_error", "safe message", errors.New("token=secret https://private.example"))
	if got := fmt.Sprint(fault); got != "upstream_error: safe message" {
		t.Fatalf("fault=%q", got)
	}
}
