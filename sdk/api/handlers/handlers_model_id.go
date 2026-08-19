package handlers

import (
	claudemodels "github.com/router-for-me/CLIProxyAPI/v7/internal/client/claude/models"
)

// canonicalizeRequestedModel splits the inbound client model ID from the
// canonical ID used for routing and upstream calls. Cloaked Anthropic listing
// IDs (claude-fable-5-dd-raw.<id> and legacy reversed) are decoded; the
// inbound string is preserved for metadata and response echo.
func canonicalizeRequestedModel(modelName string, rawJSON []byte) (inbound, canonical string, body []byte) {
	return modelName, claudemodels.ResolveClaudeModelIDPrefix(modelName), claudemodels.RewriteModelField(rawJSON)
}
