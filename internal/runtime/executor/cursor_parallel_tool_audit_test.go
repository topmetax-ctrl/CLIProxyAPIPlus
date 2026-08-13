package executor

import (
	"context"
	"strings"
	"testing"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// TestCursorOpenAIStreamSurfacesParallelToolCalls verifies that when a single
// assistant turn produces multiple MCP tool calls, the OpenAI-compatible
// streaming path surfaces every one of them (each with a distinct index) and
// closes the turn with a single tool_calls finish — instead of collapsing the
// burst to the first call, which was the pre-fix behavior.
//
// Wire capture (cmd/cursorcapture, 2026-08-13) confirmed the upstream really
// does emit N parallel exec requests in one turn before any result, so this is
// a genuine end-to-end contract, not a synthetic shape.
func TestCursorOpenAIStreamSurfacesParallelToolCalls(t *testing.T) {
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, _ func(string, bool), onToolBatch func([]pendingMcpExec), toolResultCh <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		if toolResultCh != nil {
			t.Fatalf("OpenAI streaming request unexpectedly parked an H2 session (toolResultCh != nil)")
		}
		onToolBatch([]pendingMcpExec{
			{ToolCallId: "call_1", ToolName: "get_stock_price", Args: `{"symbol":"AAPL"}`},
			{ToolCallId: "call_2", ToolName: "get_weather", Args: `{"city":"Tokyo"}`},
			{ToolCallId: "call_3", ToolName: "get_exchange_rate", Args: `{"base":"USD","quote":"EUR"}`},
		})
		return nil
	})

	result, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cursorTestRequest(true), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	body := cursorStreamPayload(collectCursorStream(t, result))

	type call struct {
		index int64
		id    string
		name  string
	}
	var calls []call
	var sawToolCallsFinish bool
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if gjson.Get(line, "choices.0.finish_reason").String() == "tool_calls" {
			sawToolCallsFinish = true
		}
		tc := gjson.Get(line, "choices.0.delta.tool_calls")
		if !tc.Exists() || len(tc.Array()) == 0 {
			continue
		}
		entry := tc.Array()[0]
		id := entry.Get("id").String()
		if id == "" {
			continue
		}
		calls = append(calls, call{
			index: entry.Get("index").Int(),
			id:    id,
			name:  entry.Get("function.name").String(),
		})
	}

	if len(calls) != 3 {
		t.Fatalf("surfaced %d tool calls, want 3\nstream=%s", len(calls), body)
	}
	wantIDs := []string{"call_1", "call_2", "call_3"}
	wantNames := []string{"get_stock_price", "get_weather", "get_exchange_rate"}
	for i, c := range calls {
		if c.index != int64(i) {
			t.Fatalf("call %d has index %d, want %d (indices must be distinct 0..N-1)", i, c.index, i)
		}
		if c.id != wantIDs[i] {
			t.Fatalf("call %d id = %q, want %q", i, c.id, wantIDs[i])
		}
		if c.name != wantNames[i] {
			t.Fatalf("call %d name = %q, want %q", i, c.name, wantNames[i])
		}
	}
	if !sawToolCallsFinish {
		t.Fatalf("stream never emitted finish_reason tool_calls\nstream=%s", body)
	}
	if strings.Contains(body, `"finish_reason":"stop"`) {
		t.Fatalf("parallel tool turn incorrectly emitted a stop finish\nstream=%s", body)
	}
}

// TestCursorExecuteNonStreamSurfacesParallelToolCalls checks the same contract
// on the non-streaming path: the response message carries all three tool_calls
// with matching ids/names and finish_reason tool_calls.
func TestCursorExecuteNonStreamSurfacesParallelToolCalls(t *testing.T) {
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, _ func(string, bool), onToolBatch func([]pendingMcpExec), toolResultCh <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		if toolResultCh != nil {
			t.Fatalf("OpenAI non-stream request unexpectedly parked an H2 session")
		}
		onToolBatch([]pendingMcpExec{
			{ToolCallId: "call_1", ToolName: "get_stock_price", Args: `{"symbol":"AAPL"}`},
			{ToolCallId: "call_2", ToolName: "get_weather", Args: `{"city":"Tokyo"}`},
			{ToolCallId: "call_3", ToolName: "get_exchange_rate", Args: `{"base":"USD","quote":"EUR"}`},
		})
		return nil
	})

	resp, err := e.Execute(context.Background(), cursorTestAuth(), cursorTestRequest(false), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", got)
	}
	calls := gjson.GetBytes(resp.Payload, "choices.0.message.tool_calls").Array()
	if len(calls) != 3 {
		t.Fatalf("surfaced %d tool calls, want 3\nbody=%s", len(calls), resp.Payload)
	}
	wantIDs := []string{"call_1", "call_2", "call_3"}
	wantNames := []string{"get_stock_price", "get_weather", "get_exchange_rate"}
	for i, c := range calls {
		if c.Get("id").String() != wantIDs[i] {
			t.Fatalf("call %d id = %q, want %q", i, c.Get("id").String(), wantIDs[i])
		}
		if c.Get("function.name").String() != wantNames[i] {
			t.Fatalf("call %d name = %q, want %q", i, c.Get("function.name").String(), wantNames[i])
		}
	}
}
