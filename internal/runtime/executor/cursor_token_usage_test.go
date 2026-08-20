package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func liveCacheHitUsage() cursorproto.TurnEndedUsage {
	return cursorproto.DecodeTurnEndedUsage(nil, cursorproto.InspectProtoFields(mustDecodeHex("08e259101618805920002812")))
}

func mustDecodeHex(s string) []byte {
	b := make([]byte, len(s)/2)
	for i := 0; i < len(b); i++ {
		hi := unhex(s[i*2])
		lo := unhex(s[i*2+1])
		b[i] = hi<<4 | lo
	}
	return b
}

func unhex(c byte) byte {
	switch {
	case c >= '0' && c <= '9':
		return c - '0'
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10
	default:
		return 0
	}
}

func TestCursorTokenUsageTerminalBeatsHeuristic(t *testing.T) {
	u := &cursorTokenUsage{}
	u.setInputEstimate(400)
	u.addOutput(7)
	in, out := u.get()
	if in != 100 || out != 7 {
		t.Fatalf("heuristic get()=(%d,%d)", in, out)
	}

	term := liveCacheHitUsage()
	if !u.settleTurnEnded(term) {
		t.Fatal("first settle should succeed")
	}
	if u.settleTurnEnded(cursorproto.TurnEndedUsage{HasInput: true, InputTokens: 1}) {
		t.Fatal("second settle must be ignored")
	}
	in, out = u.get()
	if in != 11490 || out != 22 {
		t.Fatalf("terminal get()=(%d,%d), want 11490/22", in, out)
	}
	u.addOutput(99)
	if _, out = u.get(); out != 22 {
		t.Fatalf("TokenDelta must not override terminal output, got %d", out)
	}

	usage := u.openAIUsage()
	if usage["prompt_tokens"] != int64(11490) || usage["completion_tokens"] != int64(22) {
		t.Fatalf("openai usage=%v", usage)
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"] != int64(11392) {
		t.Fatalf("cached_tokens missing: %v", usage)
	}
	if details["cache_write_tokens"] != int64(0) {
		t.Fatalf("present cache_write=0 must be emitted as known zero: %v", details)
	}
	comp, _ := usage["completion_tokens_details"].(map[string]any)
	if comp == nil || comp["reasoning_tokens"] != int64(18) {
		t.Fatalf("reasoning missing: %v", usage)
	}

	d := u.detail()
	if d.CacheReadTokens != 11392 || d.CachedTokens != 11392 || d.ReasoningTokens != 18 {
		t.Fatalf("detail=%+v", d)
	}
}

func TestCursorTokenUsageWithoutTerminalKeepsOldShape(t *testing.T) {
	u := &cursorTokenUsage{}
	u.setInputEstimate(40)
	u.addOutput(3)
	usage := u.openAIUsage()
	if _, ok := usage["prompt_tokens_details"]; ok {
		t.Fatalf("heuristic usage must omit cache details: %v", usage)
	}
	if usage["prompt_tokens"] != int64(10) || usage["completion_tokens"] != int64(3) {
		t.Fatalf("usage=%v", usage)
	}
}

func TestCursorTokenUsageHeuristicCannotOverwriteTerminal(t *testing.T) {
	u := &cursorTokenUsage{}
	if !u.settleTurnEnded(liveCacheHitUsage()) {
		t.Fatal("settle")
	}
	u.setInputEstimate(400000)
	u.addOutput(999)
	in, out := u.get()
	if in != 11490 || out != 22 {
		t.Fatalf("heuristic overwrote terminal: (%d,%d)", in, out)
	}
}

func TestCursorTokenUsageAbsentVsZeroCacheRead(t *testing.T) {
	absent := &cursorTokenUsage{}
	absent.settleTurnEnded(cursorproto.TurnEndedUsage{HasInput: true, InputTokens: 10, HasOutput: true, OutputTokens: 1})
	if _, ok := absent.openAIUsage()["prompt_tokens_details"]; ok {
		t.Fatalf("absent cache_read must omit prompt_tokens_details: %v", absent.openAIUsage())
	}

	zero := &cursorTokenUsage{}
	zero.settleTurnEnded(cursorproto.TurnEndedUsage{
		HasInput: true, InputTokens: 10,
		HasOutput: true, OutputTokens: 1,
		HasCacheRead: true, CacheReadTokens: 0,
	})
	details, _ := zero.openAIUsage()["prompt_tokens_details"].(map[string]any)
	if details == nil || details["cached_tokens"] != int64(0) {
		t.Fatalf("present zero cache_read must be emitted: %v", zero.openAIUsage())
	}
}

func TestCursorTokenUsageCacheReadExceedsInputKeepsRaw(t *testing.T) {
	u := &cursorTokenUsage{}
	u.settleTurnEnded(cursorproto.TurnEndedUsage{
		HasInput: true, InputTokens: 10,
		HasOutput: true, OutputTokens: 1,
		HasCacheRead: true, CacheReadTokens: 99,
	})
	if !u.terminal.CacheReadExceedsInput() {
		t.Fatal("expected anomaly")
	}
	usage := u.openAIUsage()
	if usage["prompt_tokens"] != int64(10) {
		t.Fatalf("must preserve raw input, got %v", usage["prompt_tokens"])
	}
	details, _ := usage["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != int64(99) {
		t.Fatalf("must preserve raw cache_read, got %v", details)
	}
}

func TestCursorTokenUsageEmptyTurnEndedDoesNotSettle(t *testing.T) {
	u := &cursorTokenUsage{}
	u.setInputEstimate(40)
	if u.settleTurnEnded(cursorproto.TurnEndedUsage{}) {
		t.Fatal("empty terminal must not settle")
	}
	in, _ := u.get()
	if in != 10 {
		t.Fatalf("input=%d, want heuristic 10", in)
	}
}

func TestCursorExecuteNonStreamEmitsTerminalCacheUsage(t *testing.T) {
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func([]pendingMcpExec), _ <-chan []toolResultInfo, usage *cursorTokenUsage, _ func([]byte)) error {
		onText("answer", false)
		usage.addOutput(7)
		usage.settleTurnEnded(liveCacheHitUsage())
		return nil
	})
	resp, err := e.Execute(context.Background(), cursorTestAuth(), cursorTestRequest(false), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.prompt_tokens").Int(); got != 11490 {
		t.Fatalf("prompt_tokens=%d", got)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.completion_tokens").Int(); got != 22 {
		t.Fatalf("completion_tokens=%d, TokenDelta must lose", got)
	}
	if got := gjson.GetBytes(resp.Payload, "usage.prompt_tokens_details.cached_tokens").Int(); got != 11392 {
		t.Fatalf("cached_tokens=%d", got)
	}
}

func TestCursorExecuteStreamEmitsTerminalCacheUsage(t *testing.T) {
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func([]pendingMcpExec), _ <-chan []toolResultInfo, usage *cursorTokenUsage, _ func([]byte)) error {
		onText("answer", false)
		usage.settleTurnEnded(liveCacheHitUsage())
		return nil
	})
	result, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cursorTestRequest(true), cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	body := cursorStreamPayload(collectCursorStream(t, result))
	if !strings.Contains(body, `"cached_tokens":11392`) {
		t.Fatalf("stream missing cached_tokens=11392 body=%s", body)
	}
	if !strings.Contains(body, `"prompt_tokens":11490`) {
		t.Fatalf("stream missing prompt_tokens=11490 body=%s", body)
	}
}

func TestCursorExecuteStreamClaudeMapsCacheRead(t *testing.T) {
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func([]pendingMcpExec), _ <-chan []toolResultInfo, usage *cursorTokenUsage, _ func([]byte)) error {
		onText("answer", false)
		usage.settleTurnEnded(liveCacheHitUsage())
		return nil
	})
	payload := []byte(`{"model":"cursor-test-model","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FromString("claude"), OriginalRequest: payload}
	result, err := e.ExecuteStream(context.Background(), cursorTestAuth(), cliproxyexecutor.Request{Model: "cursor-test-model", Payload: payload}, opts)
	if err != nil {
		t.Fatalf("ExecuteStream() error = %v", err)
	}
	body := cursorStreamPayload(collectCursorStream(t, result))
	if !strings.Contains(body, `"cache_read_input_tokens":11392`) {
		t.Fatalf("claude stream missing cache_read_input_tokens=11392:\n%s", body)
	}
	if !strings.Contains(body, `"input_tokens":98`) {
		t.Fatalf("claude stream missing uncached input_tokens=98:\n%s", body)
	}
}

func TestProcessH2SessionFramesSettlesTurnEndedOnce(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "auth", "cursor", "proto", "testdata", "cursor", "turn_ended_cache_hit.bin"))
	if err != nil {
		t.Fatal(err)
	}
	stream := newFakeCursorStream()
	usage := &cursorTokenUsage{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil)
	}()
	stream.data <- cursorproto.FrameConnectMessage(raw, 0)
	if err := <-errCh; err != nil {
		t.Fatalf("processH2SessionFrames: %v", err)
	}
	in, out := usage.get()
	if in != 11490 || out != 22 {
		t.Fatalf("settled usage=(%d,%d)", in, out)
	}
	if usage.openAIUsage()["prompt_tokens_details"].(map[string]any)["cached_tokens"] != int64(11392) {
		t.Fatalf("settled cache=%v", usage.openAIUsage())
	}
}
