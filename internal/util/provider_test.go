package util

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestGetProviderNameUnknownModelDoesNotFallbackToCursor(t *testing.T) {
	modelRegistry := registry.GetGlobalRegistry()
	const clientID = "provider-unknown-no-cursor-fallback"
	modelRegistry.RegisterClient(clientID, "cursor", []*registry.ModelInfo{
		{ID: "cursor-grok-4.6-xhigh-fast", Object: "model", OwnedBy: "cursor", Type: "cursor"},
	})
	t.Cleanup(func() {
		modelRegistry.UnregisterClient(clientID)
	})

	if got := GetProviderName("cursor-grok-4.6-xhigh-fast"); len(got) != 1 || got[0] != "cursor" {
		t.Fatalf("GetProviderName(registered) = %v, want [cursor]", got)
	}
	if got := GetProviderName("cursor-grok-4.7-typo"); len(got) != 0 {
		t.Fatalf("GetProviderName(unknown) = %v, want nil; unknown IDs must not route to Cursor", got)
	}
	if got := GetProviderName("gpt-99"); len(got) != 0 {
		t.Fatalf("GetProviderName(unknown gpt) = %v, want nil", got)
	}
}

func TestGetProviderNameEmpty(t *testing.T) {
	if got := GetProviderName(""); got != nil {
		t.Fatalf("GetProviderName(\"\") = %v, want nil", got)
	}
}
