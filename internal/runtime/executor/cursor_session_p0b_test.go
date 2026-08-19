package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
)

// Sequential request-ID correlation still holds after the generation-aware store.

func TestP0B_M1_SequentialParkRestoreMatchUsesRequestContextIDs(t *testing.T) {
	hook := newP0BLogHook(t)
	idA := normalizeToolCallID("p0b-m1-a")
	processorResult := make(chan error, 1)
	e := newCursorExecutorHarness(func(ctx context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, _ func(string, bool), onToolBatch func([]pendingMcpExec), toolResultCh <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		onToolBatch([]pendingMcpExec{{ToolCallId: idA, ToolName: "read", Args: `{}`}})
		select {
		case results := <-toolResultCh:
			if len(results) != 1 || results[0].ToolCallId != idA {
				err := fmt.Errorf("resumed unexpected results: %#v", results)
				processorResult <- err
				return err
			}
		case <-ctx.Done():
			processorResult <- ctx.Err()
			return ctx.Err()
		}
		processorResult <- nil
		return nil
	})

	beforeReplace := CursorSessionReplaceWithPendingTotal()
	sessionID := "p0b-m1-sequential"
	firstPayload := p0bClaudeUserPayload(sessionID, "turn A")
	ctxA := logging.WithRequestID(context.Background(), "req-A")
	first, err := e.ExecuteStream(ctxA, cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor-test-model", Payload: firstPayload}, p0bClaudeOpts(firstPayload))
	if err != nil {
		t.Fatalf("park ExecuteStream() error = %v", err)
	}
	_ = collectCursorStream(t, first)

	parked := p0bLastEvent(hook, "cursor_session_park")
	if parked == nil {
		t.Fatal("missing cursor_session_park")
	}
	if got := p0bString(parked, "source_request_id"); got != "req-A" {
		t.Fatalf("PARK source_request_id = %q, want req-A from request context", got)
	}
	genA := p0bString(parked, "generation_id")
	if genA == "" {
		t.Fatal("PARK generation_id is empty")
	}

	resultPayload := p0bClaudeResultPayload(sessionID, idA, "ok-A")
	ctxResult := logging.WithRequestID(context.Background(), "req-A-result")
	second, err := e.ExecuteStream(ctxResult, cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor-test-model", Payload: resultPayload}, p0bClaudeOpts(resultPayload))
	if err != nil {
		t.Fatalf("result ExecuteStream() error = %v", err)
	}
	_ = collectCursorStream(t, second)
	if err := <-processorResult; err != nil {
		t.Fatal(err)
	}

	restore := p0bLastEvent(hook, "cursor_session_restore")
	match := p0bLastEvent(hook, "cursor_tool_result_match")
	if restore == nil || match == nil {
		t.Fatalf("missing restore/match: restore=%v match=%v", restore != nil, match != nil)
	}
	if got := p0bString(restore, "consumer_request_id"); got != "req-A-result" {
		t.Fatalf("RESTORE consumer_request_id = %q, want req-A-result", got)
	}
	if got := p0bString(restore, "source_request_id"); got != "req-A" {
		t.Fatalf("RESTORE source_request_id = %q, want parked req-A", got)
	}
	if got := p0bString(restore, "generation_id"); got != genA {
		t.Fatalf("RESTORE generation_id = %q, want %q", got, genA)
	}
	if got := p0bString(match, "consumer_request_id"); got != "req-A-result" {
		t.Fatalf("MATCH consumer_request_id = %q, want req-A-result", got)
	}
	if p0bInt(match, "intersection_count") < 1 {
		t.Fatalf("M1 MATCH intersection_count = %v, want > 0", match["intersection_count"])
	}
	if CursorSessionReplaceWithPendingTotal() != beforeReplace {
		t.Fatal("M1 incremented replace_with_pending")
	}
}

func newP0BLogHook(t *testing.T) *test.Hook {
	t.Helper()
	previous := log.GetLevel()
	log.SetLevel(log.DebugLevel)
	hook := test.NewLocal(log.StandardLogger())
	t.Cleanup(func() {
		hook.Reset()
		log.SetLevel(previous)
	})
	return hook
}

func p0bLastEvent(hook *test.Hook, event string) log.Fields {
	var found log.Fields
	for _, entry := range hook.AllEntries() {
		if entry.Data["event"] == event {
			found = entry.Data
		}
	}
	return found
}

func p0bString(fields log.Fields, key string) string {
	if fields == nil {
		return ""
	}
	got, _ := fields[key].(string)
	return got
}

func p0bInt(fields log.Fields, key string) int {
	if fields == nil {
		return 0
	}
	switch got := fields[key].(type) {
	case int:
		return got
	case int64:
		return int(got)
	case uint64:
		return int(got)
	default:
		return 0
	}
}

func p0bClaudeOpts(payload []byte) cliproxyexecutor.Options {
	return cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: payload}
}

func p0bClaudeUserPayload(sessionID, text string) []byte {
	return p0bMustJSON(map[string]any{
		"model":      "cursor-test-model",
		"max_tokens": 128,
		"stream":     true,
		"metadata":   map[string]any{"user_id": p0bUserID(sessionID)},
		"messages":   []any{map[string]any{"role": "user", "content": text}},
	})
}

func p0bClaudeResultPayload(sessionID, toolID, result string) []byte {
	return p0bMustJSON(map[string]any{
		"model":      "cursor-test-model",
		"max_tokens": 128,
		"stream":     true,
		"metadata":   map[string]any{"user_id": p0bUserID(sessionID)},
		"messages": []any{
			map[string]any{"role": "user", "content": "hello"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "tool_use", "id": toolID, "name": "read", "input": map[string]any{"path": "README.md"}},
			}},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": toolID, "content": result},
			}},
		},
	})
}

func p0bUserID(sessionID string) string {
	raw, _ := json.Marshal(map[string]string{"session_id": sessionID, "device_id": "p0b"})
	return string(raw)
}

func p0bMustJSON(v any) []byte {
	raw, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return raw
}
