package model

import (
	"testing"

	"github.com/owen891/grok2api/backend/internal/domain/account"
	modeldomain "github.com/owen891/grok2api/backend/internal/domain/model"
)

func TestNewModelResponseSeparatesPublicAndUpstreamNames(t *testing.T) {
	response := newModelResponse(modeldomain.Route{
		ID: 1, PublicID: "Build/grok-4.5", Provider: account.ProviderBuild, UpstreamModel: "grok-4.5",
		Capability: modeldomain.CapabilityResponses, Enabled: true,
	})
	if response.PublicID != "grok-4.5" || response.UpstreamModel != "Build/grok-4.5" {
		t.Fatalf("model response = %#v", response)
	}
}

func TestOptionalEgressGroupIDUsesEmptyStringForDirect(t *testing.T) {
	if value, err := parseOptionalID(""); err != nil || value != 0 {
		t.Fatalf("empty group id = %d, err=%v", value, err)
	}
	if _, err := parseOptionalID("not-a-number"); err == nil {
		t.Fatal("invalid group id was accepted")
	}
	response := newModelResponse(modeldomain.Route{Provider: account.ProviderBuild})
	if response.EgressGroupID != "" {
		t.Fatalf("direct group id = %q", response.EgressGroupID)
	}
}
