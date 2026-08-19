package models

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
)

func registerResolveTestModels(t *testing.T) {
	t.Helper()
	const clientID = "claude-models-resolve-known"
	registry.GetGlobalRegistry().RegisterClient(clientID, "test", []*registry.ModelInfo{
		{ID: "gpt-4o", Object: "model", OwnedBy: "openai", Type: "openai"},
		{ID: "gemini-2.5-pro", Object: "model", OwnedBy: "google", Type: "gemini"},
		{ID: "cursor-grok-4.6-xhigh-fast", Object: "model", OwnedBy: "cursor", Type: "cursor"},
		{ID: "custom-model-x", Object: "model", OwnedBy: "test", Type: "openai"},
	})
	t.Cleanup(func() {
		registry.GetGlobalRegistry().UnregisterClient(clientID)
	})
}

func TestBuildResponse(t *testing.T) {
	availableModels := []map[string]any{
		{"id": "claude-z", "display_name": "Zebra", "max_tokens": 64000},
		{"id": "gpt-4o", "display_name": "Alpha"},
		{"id": "claude-c", "display_name": "Alpha"},
		{"id": "claude-b", "display_name": "Beta"},
	}

	response := BuildResponse(availableModels, false)
	models, ok := response["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want []map[string]any", response["data"])
	}

	wantIDs := []string{
		"claude-c",
		"claude-fable-5-dd-raw.gpt-4o",
		"claude-b",
		"claude-z",
	}
	if len(models) != len(wantIDs) {
		t.Fatalf("len(data) = %d, want %d", len(models), len(wantIDs))
	}
	for i, want := range wantIDs {
		if got, _ := models[i]["id"].(string); got != want {
			t.Fatalf("data[%d].id = %q, want %q", i, got, want)
		}
	}
	if got := models[3]["max_tokens"]; got != 64000 {
		t.Fatalf("max_tokens = %v, want 64000", got)
	}
	if got := response["has_more"]; got != false {
		t.Fatalf("has_more = %v, want false", got)
	}
	if got := response["first_id"]; got != wantIDs[0] {
		t.Fatalf("first_id = %v, want %q", got, wantIDs[0])
	}
	if got := response["last_id"]; got != wantIDs[len(wantIDs)-1] {
		t.Fatalf("last_id = %v, want %q", got, wantIDs[len(wantIDs)-1])
	}

	if got := availableModels[1]["id"]; got != "gpt-4o" {
		t.Fatalf("BuildResponse mutated input id to %v", got)
	}
	if got := availableModels[0]["id"]; got != "claude-z" {
		t.Fatalf("BuildResponse reordered input: first id = %v", got)
	}
}

func TestBuildResponseFillsEmptyDisplayName(t *testing.T) {
	response := BuildResponse([]map[string]any{
		{"id": "cursor-grok-4.6-xhigh-fast"},
	}, false)
	models, ok := response["data"].([]map[string]any)
	if !ok || len(models) != 1 {
		t.Fatalf("unexpected data: %#v", response["data"])
	}
	if got, _ := models[0]["id"].(string); got != "claude-fable-5-dd-raw.cursor-grok-4.6-xhigh-fast" {
		t.Fatalf("id = %q", got)
	}
	if got, _ := models[0]["display_name"].(string); got != "cursor-grok-4.6-xhigh-fast" {
		t.Fatalf("display_name = %q, want original id", got)
	}
}

func TestBuildResponseWithCloakingDisabled(t *testing.T) {
	availableModels := []map[string]any{
		{"id": "gpt-4o", "display_name": "GPT-4o"},
	}

	response := BuildResponse(availableModels, true)
	models, ok := response["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want []map[string]any", response["data"])
	}
	if len(models) != 1 {
		t.Fatalf("len(data) = %d, want 1", len(models))
	}
	if got := models[0]["id"]; got != "gpt-4o" {
		t.Fatalf("data[0].id = %v, want gpt-4o", got)
	}
	if got := response["first_id"]; got != "gpt-4o" {
		t.Fatalf("first_id = %v, want gpt-4o", got)
	}
	if got := response["last_id"]; got != "gpt-4o" {
		t.Fatalf("last_id = %v, want gpt-4o", got)
	}
}

func TestBuildResponseEmpty(t *testing.T) {
	response := BuildResponse(nil, false)
	models, ok := response["data"].([]map[string]any)
	if !ok {
		t.Fatalf("data type = %T, want []map[string]any", response["data"])
	}
	if len(models) != 0 {
		t.Fatalf("len(data) = %d, want 0", len(models))
	}
	if response["first_id"] != "" || response["last_id"] != "" {
		t.Fatalf("empty response IDs = (%v, %v), want empty", response["first_id"], response["last_id"])
	}
}

func TestRewriteModelField(t *testing.T) {
	registerResolveTestModels(t)
	tests := []struct {
		name string
		body string
		want string
	}{
		{"raw marker decoded", `{"model":"claude-fable-5-dd-raw.cursor-grok-4.6-xhigh-fast"}`, "cursor-grok-4.6-xhigh-fast"},
		{"legacy reversed decoded", `{"model":"claude-fable-5-dd-o4-tpg"}`, "gpt-4o"},
		{"canonical unchanged", `{"model":"cursor-grok-4.6-xhigh-fast"}`, "cursor-grok-4.6-xhigh-fast"},
		{"missing model unchanged", `{"messages":[]}`, ""},
		{"thinking suffix preserved", `{"model":"claude-fable-5-dd-raw.gpt-4o(high)"}`, "gpt-4o(high)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RewriteModelField([]byte(tt.body))
			if tt.want == "" {
				if string(got) != tt.body {
					t.Fatalf("RewriteModelField() = %s, want unchanged %s", got, tt.body)
				}
				return
			}
			if model := gjson.GetBytes(got, "model").String(); model != tt.want {
				t.Fatalf("model = %q, want %q; body=%s", model, tt.want, got)
			}
		})
	}
}

func TestEnsureClaudeModelIDPrefix(t *testing.T) {
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"empty", "", ""},
		{"already has claude prefix", "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"contains claude mid-string keeps original", "my-claude-custom", "claude-fable-5-dd-raw.my-claude-custom"},
		{"uppercase Claude prefix keeps original", "Claude-Opus-4", "claude-fable-5-dd-raw.Claude-Opus-4"},
		{"gpt model keeps original", "gpt-4o", "claude-fable-5-dd-raw.gpt-4o"},
		{"gemini model keeps original", "gemini-2.5-pro", "claude-fable-5-dd-raw.gemini-2.5-pro"},
		{"grok stays searchable", "cursor-grok-4.6-xhigh-fast", "claude-fable-5-dd-raw.cursor-grok-4.6-xhigh-fast"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EnsureClaudeModelIDPrefix(tt.id); got != tt.want {
				t.Fatalf("EnsureClaudeModelIDPrefix(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestResolveClaudeModelIDPrefix(t *testing.T) {
	registerResolveTestModels(t)
	tests := []struct {
		name string
		id   string
		want string
	}{
		{"empty", "", ""},
		{"plain claude id unchanged", "claude-sonnet-4-6", "claude-sonnet-4-6"},
		{"non encoded id unchanged", "gpt-4o", "gpt-4o"},
		{"legacy reversed gpt model", "claude-fable-5-dd-o4-tpg", "gpt-4o"},
		{"legacy reversed gemini model", "claude-fable-5-dd-orp-5.2-inimeg", "gemini-2.5-pro"},
		{"legacy unknown payload stays unchanged", "claude-fable-5-dd-special", "claude-fable-5-dd-special"},
		{"raw marker gpt model", "claude-fable-5-dd-raw.gpt-4o", "gpt-4o"},
		{"raw marker grok model", "claude-fable-5-dd-raw.cursor-grok-4.6-xhigh-fast", "cursor-grok-4.6-xhigh-fast"},
		{"empty encoded body unchanged", "claude-fable-5-dd-", "claude-fable-5-dd-"},
		{"preserves thinking suffix on legacy", "claude-fable-5-dd-o4-tpg(high)", "gpt-4o(high)"},
		{"preserves thinking suffix on raw", "claude-fable-5-dd-raw.gpt-4o(high)", "gpt-4o(high)"},
		{"round trip", EnsureClaudeModelIDPrefix("custom-model-x"), "custom-model-x"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ResolveClaudeModelIDPrefix(tt.id); got != tt.want {
				t.Fatalf("ResolveClaudeModelIDPrefix(%q) = %q, want %q", tt.id, got, tt.want)
			}
		})
	}
}

func TestEnsureResolveRoundTrip(t *testing.T) {
	registerResolveTestModels(t)
	ids := []string{
		"cursor-grok-4.6-xhigh-fast",
		"gpt-4o",
		"gemini-2.5-pro",
		"custom-model-x",
		"claude-sonnet-4-6",
		"model.with.dots",
		"model-with-hyphens",
		"gpt-4o(high)",
		"cursor-grok-4.6-xhigh-fast(xhigh)",
	}
	for _, id := range ids {
		t.Run(id, func(t *testing.T) {
			if got := ResolveClaudeModelIDPrefix(EnsureClaudeModelIDPrefix(id)); got != id {
				t.Fatalf("Resolve(Ensure(%q)) = %q, want %q", id, got, id)
			}
		})
	}
}
