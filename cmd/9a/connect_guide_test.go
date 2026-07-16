package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gopact-ai/9a/internal/declarative"
	"github.com/gopact-ai/9a/internal/provider"
)

type guideTestSink struct{ result json.RawMessage }

func (*guideTestSink) Started() error { return nil }
func (s *guideTestSink) Event(event provider.Event) error {
	if event.Type == "result" {
		s.result = append([]byte(nil), event.Data...)
	}
	return nil
}
func (*guideTestSink) Artifact(string, string, []byte) error { return nil }

func TestHTTPConnectGuideTemplateRunsAsDocumented(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/items" || r.URL.Query().Get("id") != "item-42" {
			t.Errorf("request URL=%s", r.URL.String())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"item-42"}`))
	}))
	defer server.Close()

	source := strings.Replace(httpManifestTemplate, "https://api.example.com", server.URL, 1)
	config, err := declarative.Parse([]byte(source))
	if err != nil {
		t.Fatal(err)
	}
	adapter := declarative.NewAdapter()
	p := provider.Provider{ID: "api/example-api", Protocol: "api", Name: "example-api"}
	if err := adapter.Register(p, config); err != nil {
		t.Fatal(err)
	}
	capabilities, err := adapter.Discover(context.Background(), p)
	if err != nil || len(capabilities) != 1 {
		t.Fatalf("capabilities=%#v error=%v", capabilities, err)
	}
	sink := &guideTestSink{}
	if err := adapter.Invoke(context.Background(), p, capabilities[0], "guide", json.RawMessage(`{"id":"item-42"}`), sink); err != nil {
		t.Fatal(err)
	}
	if !called || !json.Valid(sink.result) {
		t.Fatalf("called=%v result=%s", called, sink.result)
	}
}
