package capability

import "testing"

func TestStableID(t *testing.T) {
	t.Parallel()
	got := StableID("mcp", "Weather Server", "get_weather")
	if got != "mcp/weather-server/get-weather" {
		t.Fatalf("StableID() = %q", got)
	}
}

func TestValidateRequiresIdentityAndContracts(t *testing.T) {
	t.Parallel()
	if err := (Capability{}).Validate(); err == nil {
		t.Fatal("Validate() accepted empty capability")
	}
	c := Capability{
		ID: "mcp/weather/get-weather", Kind: "mcp.tool", Name: "get-weather", Description: "Get weather",
		Source: Source{Protocol: "mcp", Provider: "weather", UpstreamName: "get_weather"},
		Input:  Contract{Mode: "json"}, Output: Contract{Mode: "mcp.toolResult"},
		Lifecycle: Lifecycle{Sync: true}, Security: Security{UpstreamAuth: "provider-configured"},
	}
	if err := c.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	c.Source.UpstreamName = "!!!"
	if err := c.Validate(); err == nil {
		t.Fatal("Validate() accepted a source with no public reference")
	}
}
