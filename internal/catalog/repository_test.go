package catalog

import (
	"context"
	"testing"

	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/store"
)

func testCapability(name string) capability.Capability {
	return capability.Capability{
		ID: capability.StableID("mcp", "weather", name), Kind: "mcp.tool", Name: name, Description: "Weather " + name,
		Source: capability.Source{Protocol: "mcp", Provider: "weather", UpstreamName: name},
		Input:  capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "mcp.toolResult"},
		Lifecycle: capability.Lifecycle{Sync: true}, Security: capability.Security{UpstreamAuth: "provider-configured"},
	}
}

func TestReplaceProviderCapabilitiesIsRevisionedAndAtomic(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
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

func TestReplaceRejectsInvalidCapabilityWithoutChangingRevision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
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
	t.Cleanup(func() { db.Close() })
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
