// Package models builds model catalogs for Anthropic clients.
package models

import (
	"sort"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const claudeDDModelPrefix = "claude-fable-5-dd-"

// claudeDDRawMarker distinguishes new cloaked IDs (original name kept, so
// clients can search "grok") from legacy reversed payloads.
const claudeDDRawMarker = "raw."

// BuildResponse builds an Anthropic model response from available models.
func BuildResponse(availableModels []map[string]any, disableCloaking bool) map[string]any {
	models := make([]map[string]any, len(availableModels))
	for i, model := range availableModels {
		models[i] = cloneModel(model)
		if id, ok := models[i]["id"].(string); ok && !disableCloaking {
			cloaked := EnsureClaudeModelIDPrefix(id)
			models[i]["id"] = cloaked
			if cloaked != id {
				if displayName, _ := models[i]["display_name"].(string); strings.TrimSpace(displayName) == "" {
					models[i]["display_name"] = id
				}
			}
		}
	}

	sort.SliceStable(models, func(i, j int) bool {
		displayNameI, _ := models[i]["display_name"].(string)
		displayNameJ, _ := models[j]["display_name"].(string)
		if displayNameI != displayNameJ {
			return displayNameI < displayNameJ
		}
		idI, _ := models[i]["id"].(string)
		idJ, _ := models[j]["id"].(string)
		return idI < idJ
	})

	firstID := ""
	lastID := ""
	if len(models) > 0 {
		firstID, _ = models[0]["id"].(string)
		lastID, _ = models[len(models)-1]["id"].(string)
	}

	return map[string]any{
		"data":     models,
		"has_more": false,
		"first_id": firstID,
		"last_id":  lastID,
	}
}

// RewriteModelField decodes a cloaked listing ID in the JSON "model" field.
// Bodies without a model field, or with a canonical ID, are returned unchanged.
func RewriteModelField(rawJSON []byte) []byte {
	modelName := gjson.GetBytes(rawJSON, "model").String()
	resolved := ResolveClaudeModelIDPrefix(modelName)
	if resolved == modelName {
		return rawJSON
	}
	updated, errSet := sjson.SetBytes(rawJSON, "model", resolved)
	if errSet != nil {
		return rawJSON
	}
	return updated
}

// EnsureClaudeModelIDPrefix rewrites model IDs for Anthropic model listings.
// IDs that already start with "claude-" are returned unchanged; all other IDs
// become "claude-fable-5-dd-raw." plus the original ID so Claude Code pickers
// still see a claude- prefix while search for names like "grok" keeps working.
func EnsureClaudeModelIDPrefix(id string) string {
	if id == "" || strings.HasPrefix(id, "claude-") {
		return id
	}
	return claudeDDModelPrefix + claudeDDRawMarker + id
}

// ResolveClaudeModelIDPrefix reverses EnsureClaudeModelIDPrefix for request routing.
// Optional thinking suffixes in model(value) form are preserved.
//
// raw. cloaks always decode. Legacy character-reversed cloaks decode only when
// the reversed candidate is a registered model, so a future Anthropic ID such
// as claude-fable-5-dd-special is left unchanged instead of becoming garbage.
func ResolveClaudeModelIDPrefix(id string) string {
	if id == "" {
		return id
	}
	base, suffix, hasSuffix := splitModelThinkingSuffix(id)
	if !strings.HasPrefix(base, claudeDDModelPrefix) {
		return id
	}
	encoded := base[len(claudeDDModelPrefix):]
	if encoded == "" {
		return id
	}
	var resolved string
	if strings.HasPrefix(encoded, claudeDDRawMarker) {
		resolved = encoded[len(claudeDDRawMarker):]
	} else {
		candidate := reverseModelID(encoded)
		if !registeredModelID(candidate) {
			return id
		}
		resolved = candidate
	}
	if hasSuffix {
		return resolved + "(" + suffix + ")"
	}
	return resolved
}

func registeredModelID(id string) bool {
	if strings.TrimSpace(id) == "" {
		return false
	}
	return len(registry.GetGlobalRegistry().GetModelProviders(id)) > 0
}

func cloneModel(model map[string]any) map[string]any {
	cloned := make(map[string]any, len(model))
	for key, value := range model {
		cloned[key] = value
	}
	return cloned
}

func splitModelThinkingSuffix(model string) (base, suffix string, hasSuffix bool) {
	lastOpen := strings.LastIndex(model, "(")
	if lastOpen == -1 || !strings.HasSuffix(model, ")") {
		return model, "", false
	}
	return model[:lastOpen], model[lastOpen+1 : len(model)-1], true
}

func reverseModelID(id string) string {
	runes := []rune(id)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}
