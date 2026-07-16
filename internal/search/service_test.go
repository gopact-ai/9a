package search

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/gopact-ai/9a/internal/authz"
	"github.com/gopact-ai/9a/internal/capability"
	"github.com/gopact-ai/9a/internal/catalog"
	"github.com/gopact-ai/9a/internal/provider"
	"github.com/gopact-ai/9a/internal/store"
)

func cap(name, description string) capability.Capability {
	return capability.Capability{ID: capability.StableID("mcp", "weather", name), Kind: "mcp.tool", Name: name, Description: description,
		Source: capability.Source{Protocol: "mcp", Provider: "weather", UpstreamName: name}, Input: capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "json"}}
}

func TestSearchFiltersUnauthorizedAndSupportsText(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	catalogRepo := catalog.New(db)
	weather := cap("weather", "current weather temperature")
	secret := cap("secret", "private weather control")
	_, err = catalogRepo.ReplaceProviderCapabilities(ctx, provider.Provider{ID: "mcp/weather", Protocol: "mcp", Name: "weather"}, []capability.Capability{weather, secret})
	if err != nil {
		t.Fatal(err)
	}
	az := authz.New(db)
	if _, err := az.GrantIfAbsent(ctx, "agent", weather.ID, authz.Read); err != nil {
		t.Fatal(err)
	}
	results, err := New(db, az).Search(ctx, "agent", Query{Text: "temperature", Protocol: "mcp", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Capability.ID != weather.ID {
		t.Fatalf("results=%#v", results)
	}
	results, err = New(db, az).Search(ctx, "agent", Query{Text: "private", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("unauthorized results=%#v", results)
	}
}

func TestSearchExactShortReferenceHasStableReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := cap("weather", "weather data")
	_, err = catalog.New(db).ReplaceProviderCapabilities(ctx, provider.Provider{ID: "p", Protocol: "mcp", Name: "weather"}, []capability.Capability{c})
	if err != nil {
		t.Fatal(err)
	}
	az := authz.New(db)
	_, _ = az.GrantIfAbsent(ctx, "a", c.ID, authz.Read)
	got, err := New(db, az).Search(ctx, "a", Query{Text: "weather/weather", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "exact_ref" {
		t.Fatalf("got=%#v", got)
	}
}

func TestSearchPreservesLargeIntegersInContracts(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := cap("weather", "weather data")
	c.Input.JSONSchema = map[string]any{"const": json.Number("9007199254740993")}
	if _, err := catalog.New(db).ReplaceProviderCapabilities(ctx, provider.Provider{ID: "p", Protocol: "mcp", Name: "weather"}, []capability.Capability{c}); err != nil {
		t.Fatal(err)
	}
	az := authz.New(db)
	_, _ = az.GrantIfAbsent(ctx, "a", c.ID, authz.Read)
	got, err := New(db, az).Search(ctx, "a", Query{Text: "weather/weather", Limit: 1})
	if err != nil || len(got) != 1 {
		t.Fatalf("results=%#v error=%v", got, err)
	}
	raw, err := json.Marshal(got[0].Capability.Input.JSONSchema)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte(`9007199254740993`)) {
		t.Fatalf("search contract lost integer precision: %s", raw)
	}
}

func TestSearchFiltersByWorkspace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	visible := cap("forecast", "weather forecast")
	hidden := capability.Capability{ID: capability.StableID("mcp", "research", "forecast"), Kind: "mcp.tool", Name: "forecast", Description: "research forecast", Source: capability.Source{Protocol: "mcp", Provider: "research", UpstreamName: "forecast"}, Input: capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "json"}}
	repo := catalog.New(db)
	if _, err := repo.ReplaceProviderCapabilities(ctx, provider.Provider{ID: "mcp/weather", Protocol: "mcp", Name: "weather", Config: map[string]string{"workspace_root": "/work/a"}}, []capability.Capability{visible}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReplaceProviderCapabilities(ctx, provider.Provider{ID: "mcp/research", Protocol: "mcp", Name: "research", Config: map[string]string{"workspace_root": "/work/b"}}, []capability.Capability{hidden}); err != nil {
		t.Fatal(err)
	}
	az := authz.New(db)
	for _, id := range []string{visible.ID, hidden.ID} {
		if _, err := az.GrantIfAbsent(ctx, "agent", id, authz.Read); err != nil {
			t.Fatal(err)
		}
	}
	got, err := New(db, az).Search(ctx, "agent", Query{Text: "forecast", WorkspaceRoot: "/work/a", Limit: 10})
	if err != nil || len(got) != 1 || got[0].Capability.ID != visible.ID {
		t.Fatalf("workspace results=%#v err=%v", got, err)
	}
}

func TestSearchExactIntegrationListsAllVisibleWorkspaceCapabilities(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	makeCapability := func(id, name, description string) capability.Capability {
		return capability.Capability{
			ID: id, Kind: "mcp.tool", Name: name, Description: description,
			Source: capability.Source{Protocol: "mcp", Provider: "local-tools", UpstreamName: name},
			Input:  capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "json"},
		}
	}
	first := makeCapability("mcp/ws-a/local-tools/echo", "echo", "Echo supplied input")
	second := makeCapability("mcp/ws-a/local-tools/sum", "sum", "Add supplied numbers")
	hidden := makeCapability("mcp/ws-a/local-tools/delete", "delete", "Delete a record")
	otherWorkspace := makeCapability("mcp/ws-b/local-tools/inspect", "inspect", "Inspect a record")
	repo := catalog.New(db)
	if _, err := repo.ReplaceProviderCapabilities(ctx, provider.Provider{ID: "mcp/ws-a/local-tools", Protocol: "mcp", Name: "local-tools", Config: map[string]string{"workspace_root": "/work/a"}}, []capability.Capability{second, hidden, first}); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.ReplaceProviderCapabilities(ctx, provider.Provider{ID: "mcp/ws-b/local-tools", Protocol: "mcp", Name: "local-tools", Config: map[string]string{"workspace_root": "/work/b"}}, []capability.Capability{otherWorkspace}); err != nil {
		t.Fatal(err)
	}
	az := authz.New(db)
	for _, id := range []string{first.ID, second.ID, otherWorkspace.ID} {
		if _, err := az.GrantIfAbsent(ctx, "agent", id, authz.Read); err != nil {
			t.Fatal(err)
		}
	}

	got, err := New(db, az).Search(ctx, "agent", Query{Text: "local-tools", WorkspaceRoot: "/work/a", Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Capability.ID != first.ID || got[1].Capability.ID != second.ID {
		t.Fatalf("integration results=%#v", got)
	}
	for _, result := range got {
		if result.Reason != "integration_ref" {
			t.Fatalf("integration result reason=%q", result.Reason)
		}
	}
}

func TestSearchSingleSlugFallsBackToFullTextWithoutIntegration(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	c := capability.Capability{
		ID: "mcp/ws-a/forecast-data/current", Kind: "mcp.tool", Name: "current", Description: "Current weather observations",
		Source: capability.Source{Protocol: "mcp", Provider: "forecast-data", UpstreamName: "current"},
		Input:  capability.Contract{Mode: "json"}, Output: capability.Contract{Mode: "json"},
	}
	if _, err := catalog.New(db).ReplaceProviderCapabilities(ctx, provider.Provider{ID: "mcp/ws-a/forecast-data", Protocol: "mcp", Name: "forecast-data", Config: map[string]string{"workspace_root": "/work/a"}}, []capability.Capability{c}); err != nil {
		t.Fatal(err)
	}
	az := authz.New(db)
	if _, err := az.GrantIfAbsent(ctx, "agent", c.ID, authz.Read); err != nil {
		t.Fatal(err)
	}

	got, err := New(db, az).Search(ctx, "agent", Query{Text: "weather", WorkspaceRoot: "/work/a"})
	if err != nil || len(got) != 1 || got[0].Capability.ID != c.ID || got[0].Reason != "full_text" {
		t.Fatalf("fallback results=%#v err=%v", got, err)
	}
}
