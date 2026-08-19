package executor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// P0-C behavior regressions. These encode conversation-store invariants, not
// the one-slot map or the generation-aware implementation. They must fail on
// the current overwrite semantics and pass after pending generations coexist.

func TestCursorSession_OverlappingGenerationsPreservePending(t *testing.T) {
	idA := normalizeToolCallID("p0c-overlap-A")
	idB := normalizeToolCallID("p0c-overlap-B")
	sessionID := "p0c-overlap"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)

	live := e.livePendingToolIDs(key)
	if !p0cContains(live, idA) || !p0cContains(live, idB) {
		t.Fatalf("overlapping parks lost a pending tool: live=%v", live)
	}

	if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
		t.Fatalf("result A should resolve gen-A, not the later park: %v", err)
	}
	live = e.livePendingToolIDs(key)
	if p0cContains(live, idA) {
		t.Fatal("consumed tool A is still pending")
	}
	if !p0cContains(live, idB) {
		t.Fatal("consuming gen-A dropped gen-B pending")
	}
}

func TestCursorSession_ReverseCompletion(t *testing.T) {
	idA := normalizeToolCallID("p0c-reverse-A")
	idB := normalizeToolCallID("p0c-reverse-B")
	sessionID := "p0c-reverse"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)

	if err := p0cSendResults(e, sessionID, "req-B-result", idB); err != nil {
		t.Fatalf("result B should resolve gen-B: %v", err)
	}
	if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
		t.Fatalf("result A should still resolve gen-A after B completed: %v", err)
	}
	if live := e.livePendingToolIDs(key); len(live) != 0 {
		t.Fatalf("completed generations still have pending tools: %v", live)
	}
}

func TestCursorSession_PartialResultsAcrossNewGeneration(t *testing.T) {
	id1 := normalizeToolCallID("p0c-partial-1")
	id2 := normalizeToolCallID("p0c-partial-2")
	id3 := normalizeToolCallID("p0c-partial-3")
	idB := normalizeToolCallID("p0c-partial-B")
	sessionID := "p0c-partial"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", id1, id2, id3)

	if err := p0cSendResults(e, sessionID, "req-A-partial", id1); err != nil {
		t.Fatalf("first partial result of gen-A failed: %v", err)
	}
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)
	if err := p0cSendResults(e, sessionID, "req-A-rest", id2, id3); err != nil {
		t.Fatalf("remaining gen-A results should stay correlated after B parked: %v", err)
	}
	live := e.livePendingToolIDs(key)
	if p0cContains(live, id1) || p0cContains(live, id2) || p0cContains(live, id3) {
		t.Fatalf("gen-A tools still pending after full consume: %v", live)
	}
	if !p0cContains(live, idB) {
		t.Fatal("gen-B pending was lost while finishing gen-A")
	}
}

func TestCursorSession_NewStreamDoesNotEvictPendingGeneration(t *testing.T) {
	idA := normalizeToolCallID("p0c-newevict-A")
	idB := normalizeToolCallID("p0c-newevict-B")
	sessionID := "p0c-new-stream"
	var workers atomic.Int32
	aGot := make(chan []toolResultInfo, 1)
	e := newCursorExecutorHarness(func(ctx context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, _ func(string, bool), onToolBatch func([]pendingMcpExec), toolResultCh <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		n := workers.Add(1)
		switch n {
		case 1:
			onToolBatch([]pendingMcpExec{{ToolCallId: idA, ToolName: "read", Args: `{}`}})
			select {
			case results := <-toolResultCh:
				aGot <- results
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		case 2:
			onToolBatch([]pendingMcpExec{{ToolCallId: idB, ToolName: "read", Args: `{}`}})
			select {
			case <-toolResultCh:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		default:
			return fmt.Errorf("unexpected worker %d", n)
		}
	})

	payloadA := p0bClaudeUserPayload(sessionID, "turn A")
	first, err := e.ExecuteStream(logging.WithRequestID(context.Background(), "req-A"), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor-test-model", Payload: payloadA}, p0bClaudeOpts(payloadA))
	if err != nil {
		t.Fatalf("A park ExecuteStream() error = %v", err)
	}
	_ = collectCursorStream(t, first)

	payloadB := p0bClaudeUserPayload(sessionID, "turn B")
	second, err := e.ExecuteStream(logging.WithRequestID(context.Background(), "req-B"), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor-test-model", Payload: payloadB}, p0bClaudeOpts(payloadB))
	if err != nil {
		t.Fatalf("B new stream ExecuteStream() error = %v", err)
	}
	_ = collectCursorStream(t, second)

	if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
		t.Fatalf("new stream must not evict A's pending generation: %v", err)
	}
	select {
	case results := <-aGot:
		if len(results) != 1 || results[0].ToolCallId != idA {
			t.Fatalf("generation A received %#v", results)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("generation A was not resumed after B started")
	}
}

func TestCursorSession_ExpiredGenerationDoesNotCorruptLiveGeneration(t *testing.T) {
	idA := normalizeToolCallID("p0c-expire-A")
	idB := normalizeToolCallID("p0c-expire-B")
	sessionID := "p0c-expire"
	e, conv, key, owner := p0cExecutor(sessionID)
	genA := p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)
	if live := e.livePendingToolIDs(key); !p0cContains(live, idA) || !p0cContains(live, idB) {
		t.Fatalf("both generations must remain live before expiry: %v", live)
	}

	e.backdateGeneration(genA.generationID, cursorSessionTTL+time.Minute)
	e.expireStaleSessions()

	if state, ok := e.generationStateOf(genA.generationID); !ok || state != generationStalePending {
		t.Fatalf("soft TTL should mark gen-A STALE_PENDING, got ok=%v state=%s", ok, state)
	}
	if !p0cContains(e.livePendingToolIDs(key), idA) {
		t.Fatal("soft TTL must keep gen-A pending until hard expiry")
	}
	if !p0cContains(e.livePendingToolIDs(key), idB) {
		t.Fatal("expiring or GC-ing gen-A removed gen-B pending")
	}
	if err := p0cSendResults(e, sessionID, "req-B-result", idB); err != nil {
		t.Fatalf("live gen-B must still resolve after gen-A expiry/GC: %v", err)
	}
}

func TestCursorSession_MixedToolResultsRejected(t *testing.T) {
	idA := normalizeToolCallID("p0c-mixed-A")
	idB := normalizeToolCallID("p0c-mixed-B")
	sessionID := "p0c-mixed"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)

	err := p0cSendResults(e, sessionID, "req-mixed", idA, idB)
	if err == nil || !errors.Is(err, errMixedToolResultGenerations) && !strings.Contains(err.Error(), "MIXED_TOOL_RESULT_GENERATIONS") {
		t.Fatalf("mixed-generation results error = %v, want MIXED_TOOL_RESULT_GENERATIONS", err)
	}
	live := e.livePendingToolIDs(key)
	if !p0cContains(live, idA) || !p0cContains(live, idB) {
		t.Fatalf("mixed reject must not consume either generation: live=%v", live)
	}
}

func p0cExecutor(sessionID string) (*CursorExecutor, string, string, *cursorStateOwner) {
	e := NewCursorExecutor(nil)
	conv := deriveConversationId("", sessionID, "")
	key := "cursor-test:" + conv
	return e, conv, key, e.beginConversationStream(conv)
}

func p0cMustPark(t *testing.T, e *CursorExecutor, conv, key string, owner *cursorStateOwner, genID, reqID string, ids ...string) *cursorSession {
	t.Helper()
	pending := make([]pendingMcpExec, 0, len(ids))
	for _, id := range ids {
		pending = append(pending, pendingMcpExec{ToolCallId: id, ToolName: "read", Args: `{}`})
	}
	session := &cursorSession{
		stream:          newFakeCursorStream(),
		pending:         pending,
		cancel:          func() {},
		createdAt:       time.Now(),
		authID:          "cursor-test",
		generationID:    genID,
		sourceRequestID: reqID,
		toolResultCh:    make(chan []toolResultInfo, 8),
		resumeOutCh:     make(chan cliproxyexecutor.StreamChunk, 8),
		switchOutput:    func(chan cliproxyexecutor.StreamChunk, context.Context) {},
	}
	if !e.publishConversationSession(conv, key, owner, session, true) {
		t.Fatalf("park %s failed", genID)
	}
	go func() {
		for range session.toolResultCh {
		}
	}()
	return session
}

func p0cSendResults(e *CursorExecutor, sessionID, requestID string, ids ...string) error {
	payload := p0cClaudeResultsPayload(sessionID, ids)
	_, err := e.ExecuteStream(
		logging.WithRequestID(context.Background(), requestID),
		cursorTestAuth(),
		cliproxyexecutor.Request{Model: "cursor-test-model", Payload: payload},
		p0bClaudeOpts(payload),
	)
	return err
}

func p0cClaudeResultsPayload(sessionID string, ids []string) []byte {
	uses := make([]any, 0, len(ids))
	results := make([]any, 0, len(ids))
	for _, id := range ids {
		uses = append(uses, map[string]any{"type": "tool_use", "id": id, "name": "read", "input": map[string]any{"path": "README.md"}})
		results = append(results, map[string]any{"type": "tool_result", "tool_use_id": id, "content": "ok"})
	}
	return p0bMustJSON(map[string]any{
		"model":      "cursor-test-model",
		"max_tokens": 128,
		"stream":     true,
		"metadata":   map[string]any{"user_id": p0bUserID(sessionID)},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": uses},
			map[string]any{"role": "user", "content": results},
		},
	})
}

func p0cContains(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}
