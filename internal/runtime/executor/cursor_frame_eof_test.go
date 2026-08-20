package executor

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	"google.golang.org/protobuf/encoding/protowire"
)

func turnEndedConnectFrame(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "auth", "cursor", "proto", "testdata", "cursor", "turn_ended_cache_hit.bin"))
	if err != nil {
		t.Fatal(err)
	}
	return cursorproto.FrameConnectMessage(raw, 0)
}

func tokenDeltaConnectFrame(tokens int64) []byte {
	inner := protowire.AppendTag(nil, 1, protowire.VarintType)
	inner = protowire.AppendVarint(inner, uint64(tokens))
	iu := protowire.AppendTag(nil, cursorproto.IU_TokenDelta, protowire.BytesType)
	iu = protowire.AppendBytes(iu, inner)
	asm := protowire.AppendTag(nil, cursorproto.ASM_InteractionUpdate, protowire.BytesType)
	asm = protowire.AppendBytes(asm, iu)
	return cursorproto.FrameConnectMessage(asm, 0)
}

func TestProcessH2DrainsTurnEndedWhenDoneRaces(t *testing.T) {
	frame := turnEndedConnectFrame(t)
	stream := &fakeCursorStream{
		data: make(chan []byte, 1),
		done: make(chan struct{}),
		dead: make(chan struct{}),
	}
	stream.data <- frame
	close(stream.done)
	usage := &cursorTokenUsage{}
	if err := processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil); err != nil {
		t.Fatalf("processH2SessionFrames: %v", err)
	}
	if !usage.hasTerminal() {
		t.Fatal("TurnEnded already in dataCh must be decoded before Done() terminal handling")
	}
	in, out := usage.get()
	if in != 11490 || out != 22 {
		t.Fatalf("settled=(%d,%d)", in, out)
	}
	if _, ok := usage.openAIUsage()["prompt_tokens_details"]; !ok {
		t.Fatal("authoritative cache must survive Done() race drain")
	}
}

func TestProcessH2PartialFrameThenEOFDoesNotSettle(t *testing.T) {
	frame := turnEndedConnectFrame(t)
	stream := newFakeCursorStream()
	usage := &cursorTokenUsage{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil)
	}()
	stream.data <- frame[:3]
	close(stream.data)
	if err := <-errCh; err != nil {
		t.Fatalf("eof: %v", err)
	}
	if usage.hasTerminal() {
		t.Fatal("incomplete frame + EOF must not invent TurnEnded")
	}
}

func TestProcessH2TokenDeltaThenTurnEndedThenEOF(t *testing.T) {
	stream := newFakeCursorStream()
	usage := &cursorTokenUsage{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil)
	}()
	stream.data <- tokenDeltaConnectFrame(7)
	stream.data <- turnEndedConnectFrame(t)
	close(stream.data)
	if err := <-errCh; err != nil {
		t.Fatalf("process: %v", err)
	}
	if !usage.hasTerminal() {
		t.Fatal("expected TurnEnded")
	}
	_, out := usage.get()
	if out != 22 {
		t.Fatalf("TurnEnded output must beat TokenDelta, got %d", out)
	}
}

func TestProcessH2ConnectEndStreamWithoutTurnEnded(t *testing.T) {
	stream := newFakeCursorStream()
	usage := &cursorTokenUsage{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil)
	}()
	stream.data <- cursorproto.FrameConnectMessage(nil, cursorproto.ConnectEndStreamFlag)
	close(stream.data)
	if err := <-errCh; err != nil {
		t.Fatalf("clean connect end-stream: %v", err)
	}
	if !usage.connectEnd() {
		t.Fatal("Connect END_STREAM flag must be recorded")
	}
	if usage.hasTerminal() {
		t.Fatal("Connect END_STREAM is not TurnEnded")
	}
	if _, ok := usage.openAIUsage()["prompt_tokens_details"]; ok {
		t.Fatal("unknown cache must not be emitted as zero")
	}
}

func TestProcessH2DuplicateTurnEndedSettlesOnce(t *testing.T) {
	stream := newFakeCursorStream()
	usage := &cursorTokenUsage{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil)
	}()
	frame := turnEndedConnectFrame(t)
	stream.data <- frame
	if err := <-errCh; err != nil {
		t.Fatalf("first: %v", err)
	}
	if usage.settleTurnEnded(cursorproto.TurnEndedUsage{HasInput: true, InputTokens: 1}) {
		t.Fatal("late/duplicate TurnEnded must not resettle")
	}
}

func TestCursorTokenUsageTokenDeltaDoesNotInventCache(t *testing.T) {
	u := &cursorTokenUsage{}
	u.setInputEstimate(40)
	u.addOutput(9)
	usage := u.openAIUsage()
	if _, ok := usage["prompt_tokens_details"]; ok {
		t.Fatalf("TokenDelta-only must omit cached_tokens: %v", usage)
	}
	if usage["completion_tokens"] != int64(9) {
		t.Fatalf("TokenDelta output=%v", usage["completion_tokens"])
	}
	class, reason, _ := classifyCursorTerminal(u, "eof", nil)
	if class != cursorClassTerminalUnavailable || reason != "eof_without_turn_ended" {
		t.Fatalf("class=%s reason=%s", class, reason)
	}
}

func TestProcessH2CancelDrainsBufferedTurnEnded(t *testing.T) {
	frame := turnEndedConnectFrame(t)
	stream := &fakeCursorStream{
		data: make(chan []byte, 1),
		done: make(chan struct{}),
		dead: make(chan struct{}),
	}
	stream.data <- frame
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	usage := &cursorTokenUsage{}
	_ = processH2SessionFrames(ctx, stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil)
	if !usage.hasTerminal() {
		t.Fatal("complete frames already received must be decoded before cancel terminal handling")
	}
}

func TestProcessH2MultipleTokenDeltasThenEOF(t *testing.T) {
	stream := newFakeCursorStream()
	usage := &cursorTokenUsage{}
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, usage, nil)
	}()
	stream.data <- tokenDeltaConnectFrame(5)
	stream.data <- tokenDeltaConnectFrame(4)
	close(stream.data)
	if err := <-errCh; err != nil {
		t.Fatalf("eof: %v", err)
	}
	if usage.hasTerminal() {
		t.Fatal("TokenDelta-only must not invent TurnEnded")
	}
	_, out := usage.get()
	if out != 9 {
		t.Fatalf("output=%d want 9", out)
	}
	if _, ok := usage.openAIUsage()["prompt_tokens_details"]; ok {
		t.Fatal("unknown cache must not be emitted")
	}
}

func TestDumpCursorUsageSettledIncludesLastFrames(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("CURSOR_WIRE_DUMP_DIR", dir)
	u := &cursorTokenUsage{}
	u.noteFrame("token", 0)
	u.noteFrame("connect_end_stream", 2)
	publishCursorUsageSettlement(u, "eof", nil)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var found map[string]any
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "-usage_settled.json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, &found); err != nil {
			t.Fatal(err)
		}
	}
	frames, _ := found["last_frames"].([]any)
	if len(frames) != 2 {
		t.Fatalf("last_frames=%v", found["last_frames"])
	}
}

func TestRecordCursorUsageSettlementIncrementsCoverage(t *testing.T) {
	before := CursorUsageMetricsSnapshot()
	u := &cursorTokenUsage{}
	u.settleTurnEnded(liveCacheHitUsage())
	publishCursorUsageSettlement(u, "stop", nil)
	after := CursorUsageMetricsSnapshot()
	if after.TurnEnded <= before.TurnEnded {
		t.Fatalf("turn_ended counter did not increase: %+v -> %+v", before, after)
	}
	if after.CacheObservable <= before.CacheObservable {
		t.Fatalf("cache observable did not increase")
	}
	if after.CacheReadTokens <= before.CacheReadTokens {
		t.Fatalf("cache_read total did not increase")
	}
}
