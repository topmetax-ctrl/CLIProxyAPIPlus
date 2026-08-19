package executor

import (
	"strings"
	"testing"
	"time"
)

func TestCursorToolIDSetHashDoesNotEmbedRawIDs(t *testing.T) {
	raw := "cursor_call_Y2FsbC1jY2U4NjBlNi1hYjA3LTQxNGQtODEyYy03ODVkYjM1YjE3Y2EtNA"
	hash := cursorToolIDSetHash([]string{raw})
	if hash == "" || strings.Contains(hash, raw) || strings.Contains(hash, "cursor_call_") {
		t.Fatalf("set hash leaked raw tool ID: %q", hash)
	}
	if cursorToolIDSetHash([]string{raw, raw}) != hash {
		t.Fatal("duplicate IDs changed the set hash")
	}
	if cursorToolIDIntersectionCount([]string{"a", "b"}, []string{"b", "c", "b"}) != 1 {
		t.Fatal("intersection should count unique pending IDs present in incoming")
	}
	if cursorToolIDIntersectionCount([]string{"a"}, []string{"b"}) != 0 {
		t.Fatal("disjoint sets should have intersection 0")
	}
}

func TestCursorSessionOverlappingParkDoesNotReplacePending(t *testing.T) {
	e := NewCursorExecutor(nil)
	conversationID := "trace-conversation"
	sessionKey := "cursor-test:" + conversationID
	owner := e.beginConversationStream(conversationID)

	beforeReplace := CursorSessionReplaceWithPendingTotal()
	first := &cursorSession{
		pending:   []pendingMcpExec{{ToolCallId: "cursor_call_pendingA"}},
		cancel:    func() {},
		createdAt: time.Now(),
	}
	if !e.publishConversationSession(conversationID, sessionKey, owner, first, true) {
		t.Fatal("first park failed")
	}
	second := &cursorSession{
		pending:   []pendingMcpExec{{ToolCallId: "cursor_call_pendingB"}},
		cancel:    func() {},
		createdAt: time.Now(),
	}
	if !e.publishConversationSession(conversationID, sessionKey, owner, second, true) {
		t.Fatal("second park failed")
	}
	if CursorSessionReplaceWithPendingTotal() != beforeReplace {
		t.Fatal("overlapping park incremented replace-with-pending")
	}
	live := e.livePendingToolIDs(sessionKey)
	if !p0cContains(live, "cursor_call_pendingA") || !p0cContains(live, "cursor_call_pendingB") {
		t.Fatalf("overlapping park lost a pending tool: %v", live)
	}
}

func TestCursorSessionReparkKeepsSameGeneration(t *testing.T) {
	e := NewCursorExecutor(nil)
	conversationID := "restore-no-park"
	sessionKey := "cursor-test:" + conversationID
	owner := e.beginConversationStream(conversationID)
	session := &cursorSession{
		pending:   []pendingMcpExec{{ToolCallId: "cursor_call_pendingA"}},
		cancel:    func() {},
		createdAt: time.Now(),
	}
	if !e.publishConversationSession(conversationID, sessionKey, owner, session, true) {
		t.Fatal("park failed")
	}
	if !e.publishConversationSession(conversationID, sessionKey, owner, session, false) {
		t.Fatal("repark of the same generation failed")
	}
	if !e.hasParkedSession(sessionKey, session) {
		t.Fatal("repark dropped the generation")
	}
}
