package search

import (
	"context"
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
	t.Cleanup(func() { db.Close() })
	catalogRepo := catalog.New(db)
	weather := cap("weather", "current weather temperature")
	secret := cap("secret", "private weather control")
	_, err = catalogRepo.ReplaceProviderCapabilities(ctx, provider.Provider{ID: "mcp/weather", Protocol: "mcp", Name: "weather"}, []capability.Capability{weather, secret})
	if err != nil {
		t.Fatal(err)
	}
	az := authz.New(db)
	if err := az.Grant(ctx, "agent", weather.ID, authz.Read); err != nil {
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

func TestSearchExactIDHasStableReason(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := store.Open(ctx, t.TempDir()+"/ninea.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	c := cap("weather", "weather data")
	_, err = catalog.New(db).ReplaceProviderCapabilities(ctx, provider.Provider{ID: "p", Protocol: "mcp", Name: "weather"}, []capability.Capability{c})
	if err != nil {
		t.Fatal(err)
	}
	az := authz.New(db)
	_ = az.Grant(ctx, "a", c.ID, authz.Read)
	got, err := New(db, az).Search(ctx, "a", Query{Text: c.ID, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Reason != "exact_id" {
		t.Fatalf("got=%#v", got)
	}
}
