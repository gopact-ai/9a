package declarative

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
)

func TestDeclarativeHookHelperProcess(t *testing.T) {
	if os.Getenv("NINEA_DECLARATIVE_HOOK_HELPER") != "1" {
		return
	}
	if os.Getenv("WEATHER_CLIENT") != "hook-client" || os.Getenv("HOME") != "" {
		os.Exit(3)
	}
	if os.Getenv("NINEA_DECLARATIVE_HOOK_FAIL") == "1" {
		_, _ = os.Stderr.WriteString("hook-stderr-secret")
		os.Exit(7)
	}
	if _, err := io.ReadAll(os.Stdin); err != nil {
		os.Exit(4)
	}
	_, _ = os.Stdout.WriteString(`{"hooked":true}`)
	os.Exit(0)
}

type recordingSink struct {
	started bool
	result  json.RawMessage
}

func (s *recordingSink) Started() error {
	s.started = true
	return nil
}

func (s *recordingSink) Event(event provider.Event) error {
	if event.Type == "result" {
		s.result = append([]byte(nil), event.Data...)
	}
	return nil
}

func (*recordingSink) Artifact(string, string, []byte) error { return nil }

func TestAdapterInvokesHTTPAndRunsDeclarativeHooks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/forecast" || r.URL.Query().Get("latitude") != "31.2" {
			t.Errorf("request URL=%s", r.URL.String())
		}
		if got := r.Header.Get("X-Client"); got != "test-suite" {
			t.Errorf("X-Client=%q", got)
		}
		if got := r.Header.Get("X-Trace"); got != "test-suite" {
			t.Errorf("X-Trace=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"current":{"temperature_2m":26.5}}`))
	}))
	defer server.Close()

	source := strings.ReplaceAll(validSource, "https://api.open-meteo.com", server.URL)
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEATHER_CLIENT", "test-suite")
	adapter := NewAdapter()
	providerConfig := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(providerConfig, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), providerConfig)
	if err != nil {
		t.Fatal(err)
	}
	if len(capabilities) != 2 {
		t.Fatalf("capabilities=%#v", capabilities)
	}

	var current capability.Capability
	for _, item := range capabilities {
		if item.Source.UpstreamName == "current" {
			current = item
		}
	}
	if current.ID != "api/weather/current" {
		t.Fatalf("current=%#v", current)
	}
	sink := &recordingSink{}
	if err := adapter.Invoke(context.Background(), providerConfig, current, "call-1", json.RawMessage(`{"latitude":31.2}`), sink); err != nil {
		t.Fatal(err)
	}
	if !sink.started || string(sink.result) != `{"temperature":26.5}` {
		t.Fatalf("started=%v result=%s", sink.started, sink.result)
	}
}

func TestAdapterRejectsMissingRequiredVariable(t *testing.T) {
	source := strings.Replace(validSource, "required: false", "required: true", 1)
	if source == validSource {
		source = strings.Replace(validSource, "    default: ninea", "    required: true", 1)
	}
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	err = adapter.Invoke(context.Background(), p, capabilities[0], "call-1", json.RawMessage(`{}`), sink)
	if err == nil || !strings.Contains(err.Error(), "WEATHER_CLIENT") {
		t.Fatalf("error=%v", err)
	}
}

func TestDiscoveryMarksMutatingOperationsAndWorkflowsForApproval(t *testing.T) {
	config, err := Parse([]byte(strings.Replace(validSource, "method: GET", "method: POST", 1)))
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range capabilities {
		if item.Security.RequiresApproval != "always" {
			t.Fatalf("%s approval=%q", item.ID, item.Security.RequiresApproval)
		}
	}
}

func TestDiscoveryTreatsLowercaseGETAsReadOnly(t *testing.T) {
	config, err := Parse([]byte(strings.Replace(validSource, "method: GET", "method: get", 1)))
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range capabilities {
		if item.Security.RequiresApproval != "never" {
			t.Fatalf("%s approval=%q", item.ID, item.Security.RequiresApproval)
		}
	}
}

func TestAdapterRunsBoundedExecutableHook(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(validSource, "https://api.open-meteo.com", server.URL)
	source = strings.Replace(source, "    default: ninea", "    required: true", 1)
	source = strings.Replace(source, "        - transform:\n            language: jq\n            expression: '{temperature: .body.current.temperature_2m}'", fmt.Sprintf("        - exec:\n            command: [%q, -test.run=^TestDeclarativeHookHelperProcess$]\n            env: [WEATHER_CLIENT, NINEA_DECLARATIVE_HOOK_HELPER]\n            timeout: 2s\n            maxOutputBytes: 1024", executable), 1)
	source += "security:\n  allowExecutableHooks: true\n"
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEATHER_CLIENT", "hook-client")
	t.Setenv("NINEA_DECLARATIVE_HOOK_HELPER", "1")
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var current capability.Capability
	for _, item := range capabilities {
		if item.Source.UpstreamName == "current" {
			current = item
		}
	}
	sink := &recordingSink{}
	if err := adapter.Invoke(context.Background(), p, current, "call-hook", json.RawMessage(`{"latitude":31.2}`), sink); err != nil {
		t.Fatal(err)
	}
	if string(sink.result) != `{"hooked":true}` {
		t.Fatalf("result=%s", sink.result)
	}
}

func TestExecutableHookAdmissionIsBounded(t *testing.T) {
	for i := 0; i < cap(executableHookSlots); i++ {
		executableHookSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < cap(executableHookSlots); i++ {
			<-executableHookSlots
		}
	}()
	_, err := runExecutableHook(context.Background(), ExecHook{Command: []string{"/bin/true"}}, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "capacity") {
		t.Fatalf("error=%v", err)
	}
}

func TestExecutableHookFailureRedactsStderr(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("WEATHER_CLIENT", "hook-client")
	t.Setenv("NINEA_DECLARATIVE_HOOK_HELPER", "1")
	t.Setenv("NINEA_DECLARATIVE_HOOK_FAIL", "1")
	_, err = runExecutableHook(context.Background(), ExecHook{
		Command: []string{executable, "-test.run=^TestDeclarativeHookHelperProcess$"},
		Env:     []string{"WEATHER_CLIENT", "NINEA_DECLARATIVE_HOOK_HELPER", "NINEA_DECLARATIVE_HOOK_FAIL"},
	}, map[string]any{})
	if err == nil || strings.Contains(err.Error(), "hook-stderr-secret") || strings.Contains(err.Error(), executable) {
		t.Fatalf("error=%v", err)
	}
}

func TestJQFailureRedactsInputValues(t *testing.T) {
	_, err := runJQ(".value.foo", map[string]any{"value": "transform-secret"})
	if err == nil || strings.Contains(err.Error(), "transform-secret") {
		t.Fatalf("error=%v", err)
	}
}

func TestAdapterRunsWorkflowSteps(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("latitude") != "40.7" {
			t.Errorf("query=%s", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"current":{"temperature_2m":18}}`))
	}))
	defer server.Close()
	config, err := Parse([]byte(strings.ReplaceAll(validSource, "https://api.open-meteo.com", server.URL)))
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var workflow capability.Capability
	for _, item := range capabilities {
		if item.Source.UpstreamName == "report" {
			workflow = item
		}
	}
	sink := &recordingSink{}
	if err := adapter.Invoke(context.Background(), p, workflow, "call-workflow", json.RawMessage(`{"latitude":40.7}`), sink); err != nil {
		t.Fatal(err)
	}
	if string(sink.result) != `{"temperature":18}` {
		t.Fatalf("result=%s", sink.result)
	}
}

func TestAdapterRejectsCrossOriginRedirect(t *testing.T) {
	receivedAuthorization := false
	destination := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		receivedAuthorization = r.Header.Get("Authorization") != ""
	}))
	defer destination.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, destination.URL, http.StatusFound)
	}))
	defer origin.Close()
	source := strings.ReplaceAll(validSource, "https://api.open-meteo.com", origin.URL)
	source = strings.Replace(source, "    baseURL: "+origin.URL, "    baseURL: "+origin.URL+"\n    headers:\n      Authorization: Bearer-secret", 1)
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var current capability.Capability
	for _, item := range capabilities {
		if item.Source.UpstreamName == "current" {
			current = item
		}
	}
	err = adapter.Invoke(context.Background(), p, current, "redirect", json.RawMessage(`{"latitude":1}`), &recordingSink{})
	if err == nil || receivedAuthorization {
		t.Fatalf("error=%v leaked=%v", err, receivedAuthorization)
	}
}

func TestAdapterRemovesHeadersCaseInsensitively(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("Authorization=%q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"current":{"temperature_2m":1}}`))
	}))
	defer server.Close()
	source := strings.ReplaceAll(validSource, "https://api.open-meteo.com", server.URL)
	source = strings.Replace(source, "    baseURL: "+server.URL, "    baseURL: "+server.URL+"\n    headers:\n      Authorization: secret", 1)
	source = strings.Replace(source, "beforeRequest:\n        - setHeaders:\n            X-Trace: \"{{ vars.client }}\"", "beforeRequest:\n        - transform:\n            language: jq\n            expression: '.headers = {\"authorization\": \"secret\"}'\n        - removeHeaders: [authorization]", 1)
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var current capability.Capability
	for _, item := range capabilities {
		if item.Source.UpstreamName == "current" {
			current = item
		}
	}
	if err := adapter.Invoke(context.Background(), p, current, "headers", json.RawMessage(`{"latitude":1}`), &recordingSink{}); err != nil {
		t.Fatal(err)
	}
}

func TestAdapterRedactsHTTPErrorURLAndQuery(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	endpoint := "http://" + listener.Addr().String()
	_ = listener.Close()
	source := strings.ReplaceAll(validSource, "https://api.open-meteo.com", endpoint)
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var current capability.Capability
	for _, item := range capabilities {
		if item.Source.UpstreamName == "current" {
			current = item
		}
	}
	err = adapter.Invoke(context.Background(), p, current, "redact", json.RawMessage(`{"latitude":"query-secret"}`), &recordingSink{})
	if err == nil || strings.Contains(err.Error(), "query-secret") || strings.Contains(err.Error(), endpoint) {
		t.Fatalf("error=%v", err)
	}
}

func TestAdapterRejectsNonObjectQueryFromHook(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	defer server.Close()
	source := strings.ReplaceAll(validSource, "https://api.open-meteo.com", server.URL)
	source = strings.Replace(source, "beforeRequest:\n        - setHeaders:\n            X-Trace: \"{{ vars.client }}\"", "beforeRequest:\n        - transform:\n            language: jq\n            expression: '.query = \"invalid\"'", 1)
	config, err := Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	adapter := NewAdapter()
	p := provider.Provider{ID: "api/weather", Protocol: "api", Name: "weather"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil {
		t.Fatal(err)
	}
	var current capability.Capability
	for _, item := range capabilities {
		if item.Source.UpstreamName == "current" {
			current = item
		}
	}
	err = adapter.Invoke(context.Background(), p, current, "bad-query", json.RawMessage(`{"latitude":1}`), &recordingSink{})
	if err == nil || !strings.Contains(err.Error(), "query") || called {
		t.Fatalf("error=%v called=%v", err, called)
	}
}
