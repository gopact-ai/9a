package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/store"
)

func TestCapabilitySchemaPreservesLargeIntegers(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := New(db)
	p := provider.Provider{ID: "mcp/weather", Protocol: "mcp", Name: "weather"}
	c := testCapability("forecast")
	c.Input.JSONSchema = map[string]any{"const": json.Number("9007199254740993")}
	if _, err := repo.ReplaceProviderCapabilities(ctx, p, []capability.Capability{c}); err != nil {
		t.Fatal(err)
	}
	got, err := repo.GetCapability(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(got.Input.JSONSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`9007199254740993`)) {
		t.Fatalf("persisted schema lost integer precision: %s", raw)
	}
}

func testCapability(name string) capability.Capability {
	return capability.Capability{
		ID: capability.StableID("mcp", "weather", name), Kind: "mcp.tool", Name: name, Description: "Weather " + name,
		Source: capability.Source{Protocol: "mcp", Provider: "weather", UpstreamName: name},
		Input:  capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "mcp.toolResult"},
		Lifecycle: capability.Lifecycle{Sync: true}, Security: capability.Security{UpstreamAuth: "provider-configured"},
	}
}

func TestResolveWorkspaceCapabilityUsesShortReferenceAndRejectsAmbiguity(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := New(db)

	mcpCapability := testCapability("Get Weather")
	mcpProvider := provider.Provider{ID: "mcp/ws-0000000000000000/weather", Protocol: "mcp", Name: "weather", Config: map[string]string{"workspace_root": "/work/weather"}}
	mcpCapability.ID = mcpProvider.ID + "/get-weather"
	if _, err := r.ReplaceProviderCapabilities(ctx, mcpProvider, []capability.Capability{mcpCapability}); err != nil {
		t.Fatal(err)
	}
	resolved, err := r.ResolveWorkspaceCapability(ctx, "/work/weather", "weather/get-weather")
	if err != nil || resolved.ID != mcpCapability.ID {
		t.Fatalf("ResolveWorkspaceCapability()=%#v, %v", resolved, err)
	}
	if _, err := r.ResolveWorkspaceCapability(ctx, "/work/weather", "weather/missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing ResolveWorkspaceCapability() error=%v", err)
	}
	if _, err := r.ResolveWorkspaceCapability(ctx, "/another/workspace", "weather/get-weather"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-workspace ResolveWorkspaceCapability() error=%v", err)
	}
	if _, err := r.ResolveWorkspaceCapability(ctx, "/work/weather", mcpCapability.ID); err == nil {
		t.Fatal("ResolveWorkspaceCapability() accepted an internal capability id")
	}

	a2aCapability := mcpCapability
	a2aCapability.ID = "a2a/ws-1111111111111111/weather/get-weather"
	a2aCapability.Source.Protocol = "a2a"
	a2aProvider := provider.Provider{ID: "a2a/ws-1111111111111111/weather", Protocol: "a2a", Name: "weather", Config: map[string]string{"workspace_root": "/work/weather"}}
	if _, err := r.ReplaceProviderCapabilities(ctx, a2aProvider, []capability.Capability{a2aCapability}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ResolveWorkspaceCapability(ctx, "/work/weather", "weather/get-weather"); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("ambiguous ResolveWorkspaceCapability() error=%v", err)
	}
}

func TestReplaceProviderCapabilitiesIsRevisionedAndAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := New(db)
	p := provider.Provider{ID: "mcp/weather", Protocol: "mcp", Name: "weather", Endpoint: "stdio:/bin/weather"}
	if rev, err := r.ReplaceProviderCapabilities(ctx, p, []capability.Capability{testCapability("current"), testCapability("forecast")}); err != nil || rev != 1 {
		t.Fatalf("first replace: rev=%d err=%v", rev, err)
	}
	if rev, err := r.ReplaceProviderCapabilities(ctx, p, []capability.Capability{testCapability("forecast")}); err != nil || rev != 2 {
		t.Fatalf("second replace: rev=%d err=%v", rev, err)
	}
	got, err := r.ListCapabilities(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "forecast" || got[0].Revision != 2 {
		t.Fatalf("capabilities = %#v", got)
	}
	if _, err := r.GetCapability(ctx, testCapability("current").ID); err == nil {
		t.Fatal("removed capability still exists")
	}
}

func TestReplaceRevokesOnlyRemovedCapabilityACLs(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	r := New(db)
	p := provider.Provider{ID: "mcp/weather", Protocol: "mcp", Name: "weather"}
	current, forecast := testCapability("current"), testCapability("forecast")
	if _, err := r.ReplaceProviderCapabilities(ctx, p, []capability.Capability{current, forecast}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO acl(identity_id,capability_id,permission) VALUES('agent',?,'invoke'),('agent',?,'invoke')`, current.ID, forecast.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReplaceProviderCapabilities(ctx, p, []capability.Capability{forecast}); err != nil {
		t.Fatal(err)
	}
	var removed, retained int
	if err := db.QueryRow(`SELECT count(*) FROM acl WHERE capability_id=?`, current.ID).Scan(&removed); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM acl WHERE capability_id=?`, forecast.ID).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if removed != 0 || retained != 1 {
		t.Fatalf("removed=%d retained=%d", removed, retained)
	}
}

func TestReplaceRejectsInvalidCapabilityWithoutChangingRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := New(db)
	p := provider.Provider{ID: "mcp/weather", Protocol: "mcp", Name: "weather"}
	if _, err := r.ReplaceProviderCapabilities(ctx, p, []capability.Capability{{}}); err == nil {
		t.Fatal("invalid capability accepted")
	}
	if rev, err := r.Revision(ctx); err != nil || rev != 0 {
		t.Fatalf("revision = %d, err = %v", rev, err)
	}
}

func TestListProvidersRestoresPersistedRegistration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	r := New(db)
	p := provider.Provider{ID: "mcp/weather", Protocol: "mcp", Name: "weather", Endpoint: "stdio:/bin/weather"}
	if _, err := r.ReplaceProviderCapabilities(ctx, p, []capability.Capability{testCapability("forecast")}); err != nil {
		t.Fatal(err)
	}
	got, err := r.ListProviders(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Endpoint != p.Endpoint {
		t.Fatalf("providers=%#v", got)
	}
}
