package executor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	cursorClassTurnEndedMatched    = "TURN_ENDED_MATCHED"
	cursorClassNoTurnEndedExpected = "NO_TURN_ENDED_EXPECTED"
	cursorClassTerminalUnavailable = "TERMINAL_USAGE_UNAVAILABLE_BY_PROTOCOL"
	cursorClassRealFailure         = "REAL_FAILURE"
	cursorContinuationSeamEnv      = "CURSOR_CONTINUATION_SEAM"
	cursorContinuationModeEnv      = "CURSOR_CONTINUATION_MODE"
	cursorContinuationModeHeader   = "X-Cursor-Continuation-Mode"
)

func newCursorAudit(conversationID, sessionID, model, continuity string) cursorAuditMeta {
	return cursorAuditMeta{
		AuditID:        uuid.New().String(),
		ConversationID: conversationID,
		SessionID:      sessionID,
		Model:          model,
		Continuity:     continuity,
	}
}

func continuationSeamEnabled() bool {
	return strings.TrimSpace(os.Getenv(cursorContinuationSeamEnv)) != ""
}

func continuationModeFromOptions(opts cliproxyexecutor.Options) string {
	if !continuationSeamEnabled() {
		return "auto"
	}
	if opts.Headers != nil {
		switch v := strings.ToLower(strings.TrimSpace(opts.Headers.Get(cursorContinuationModeHeader))); v {
		case "warm", "cold":
			return v
		}
	}
	switch v := strings.ToLower(strings.TrimSpace(os.Getenv(cursorContinuationModeEnv))); v {
	case "warm", "cold":
		return v
	default:
		return "auto"
	}
}

// planCursorContinuation decides checkpoint vs flatten without changing request
// bytes by itself. mode is auto|warm|cold. cold tool continuation still
// flattens unless the debug seam forces warm and a same-auth checkpoint exists.
func planCursorContinuation(mode string, coldTool, hasCheckpoint, sameAuth, hasTurns bool) (useCheckpoint, flatten bool, continuity string) {
	switch mode {
	case "cold":
		if hasTurns || hasCheckpoint || coldTool {
			return false, true, "flatten"
		}
		return false, false, "fresh"
	case "warm":
		if hasCheckpoint && sameAuth {
			return true, false, "checkpoint"
		}
	}
	switch {
	case coldTool:
		return false, true, "cold_continuation"
	case hasCheckpoint && sameAuth:
		return true, false, "checkpoint"
	case hasTurns:
		return false, true, "flatten"
	default:
		return false, false, "fresh"
	}
}

func classifyCursorTerminal(usage *cursorTokenUsage, finish string, streamErr error) (class, reason string, expected bool) {
	switch finish {
	case "tool_calls":
		return cursorClassNoTurnEndedExpected, "tool_call_boundary_waiting_for_resume", false
	case "cancel":
		return cursorClassTerminalUnavailable, "cancelled", false
	case "error":
		if streamErr != nil {
			return cursorClassRealFailure, "stream_error", true
		}
		return cursorClassRealFailure, "error", true
	}
	if usage != nil && usage.hasTerminal() {
		return cursorClassTurnEndedMatched, "turn_ended_settled", true
	}
	if finish == "eof" {
		return cursorClassTerminalUnavailable, "eof_without_turn_ended", true
	}
	return cursorClassTerminalUnavailable, "stop_without_turn_ended", true
}

func dumpCursorUsageSettled(meta cursorAuditMeta, usage *cursorTokenUsage, finish, class, reason string, terminalExpected bool, streamErr error) {
	dir := strings.TrimSpace(os.Getenv("CURSOR_WIRE_DUMP_DIR"))
	if dir == "" {
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	payload := map[string]any{
		"event_type":        "usage_settled",
		"timestamp":         time.Now().UTC().Format(time.RFC3339Nano),
		"audit_id":          meta.AuditID,
		"conversation_id":   cursorproto.ConversationShort(meta.ConversationID),
		"session_id":        meta.SessionID,
		"model":             meta.Model,
		"continuity":        meta.Continuity,
		"finish":            finish,
		"classification":    class,
		"reason":            reason,
		"terminal_expected": terminalExpected,
	}
	if streamErr != nil {
		payload["error"] = streamErr.Error()
	}
	if usage != nil {
		usage.mu.Lock()
		payload["usage_source"] = usage.sourceLocked()
		payload["terminal_seen"] = usage.terminalSeen
		payload["cache_read_anomaly"] = usage.cacheReadExceedsInputLocked()
		if usage.terminalSeen {
			if usage.terminal.HasInput {
				payload["input_tokens"] = usage.terminal.InputTokens
			}
			if usage.terminal.HasOutput {
				payload["output_tokens"] = usage.terminal.OutputTokens
			}
			if usage.terminal.HasCacheRead {
				payload["cache_read_tokens"] = usage.terminal.CacheReadTokens
				payload["cache_read_present"] = true
			}
			if usage.terminal.HasCacheWrite {
				payload["cache_write_tokens"] = usage.terminal.CacheWriteTokens
				payload["cache_write_present"] = true
			}
			if usage.terminal.HasReasoning {
				payload["reasoning_tokens"] = usage.terminal.ReasoningTokens
				payload["reasoning_present"] = true
			}
		} else {
			payload["input_tokens"] = usage.inputLocked()
			payload["output_tokens"] = usage.outputLocked()
		}
		usage.mu.Unlock()
	} else {
		payload["usage_source"] = "payload_estimate"
		payload["terminal_seen"] = false
		payload["cache_read_anomaly"] = false
	}
	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	name := time.Now().UTC().Format("150405.000") + "-" + cursorproto.ConversationShort(meta.AuditID) + "-usage_settled.json"
	if err := os.WriteFile(filepath.Join(dir, name), append(b, '\n'), 0o644); err != nil {
		log.Debugf("cursor: usage_settled dump failed: %v", err)
	}
	source, _ := payload["usage_source"].(string)
	log.Debugf("cursor: usage_source=%s classification=%s reason=%s audit=%s finish=%s terminal_expected=%t",
		source, class, reason, cursorproto.ConversationShort(meta.AuditID), finish, terminalExpected)
}

func publishCursorUsageSettlement(usage *cursorTokenUsage, finish string, streamErr error) (class, reason string, expected bool) {
	class, reason, expected = classifyCursorTerminal(usage, finish, streamErr)
	meta := cursorAuditMeta{}
	if usage != nil {
		meta = usage.auditCopy()
	}
	dumpCursorUsageSettled(meta, usage, finish, class, reason, expected, streamErr)
	return class, reason, expected
}
