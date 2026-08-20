package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
)

func thinkingDeltaFrame(text string) []byte {
	inner := append([]byte{0x0a, byte(len(text))}, []byte(text)...)
	mid := append([]byte{0x22, byte(len(inner))}, inner...)
	outer := append([]byte{0x0a, byte(len(mid))}, mid...)
	return cursorproto.FrameConnectMessage(outer, 0)
}

func turnEndedFrame() []byte {
	return cursorproto.FrameConnectMessage([]byte{0x0a, 0x02, 0x72, 0x00}, 0)
}

func withShortToolBatchIdle(t *testing.T, d time.Duration) {
	t.Helper()
	prev := cursorToolBatchIdle
	cursorToolBatchIdle = d
	t.Cleanup(func() { cursorToolBatchIdle = prev })
}

func withShadowThresholds(t *testing.T, th ...time.Duration) {
	t.Helper()
	prev := cursorShadowStallThresholdsForTest
	cursorShadowStallThresholdsForTest = th
	t.Cleanup(func() { cursorShadowStallThresholdsForTest = prev })
}

type stallObserverHold struct {
	o *cursorStallObserver
}

func (h *stallObserverHold) snapshot() cursorStallSnapshot {
	if h == nil || h.o == nil {
		return cursorStallSnapshot{}
	}
	return h.o.snapshot()
}

func attachStallObserver(t *testing.T) *stallObserverHold {
	t.Helper()
	h := &stallObserverHold{}
	cursorStallObserverHook = func(o *cursorStallObserver) { h.o = o }
	t.Cleanup(func() { cursorStallObserverHook = nil })
	return h
}

func sendFrame(t *testing.T, stream *fakeCursorStream, errCh <-chan error, frame []byte) {
	t.Helper()
	select {
	case stream.data <- frame:
	case err := <-errCh:
		t.Fatalf("frame processor exited early: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out sending frame")
	}
}

func framedParallelFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "auth", "cursor", "proto", "testdata", "cursor-2026-08", "parallel", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return cursorproto.FrameConnectMessage(b, 0)
}

func waitProcessor(t *testing.T, errCh <-chan error, timeout time.Duration) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(timeout):
		t.Fatal("timed out waiting for frame processor")
		return nil
	}
}

func TestCursorStallT1HealthySemanticDoesNotAbort(t *testing.T) {
	withShortStallTimeout(t, 200*time.Millisecond)
	withShadowThresholds(t, 50*time.Millisecond)
	obsPtr := attachStallObserver(t)

	stream := newFakeCursorStream()
	errCh := make(chan error, 1)
	var got string
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil,
			func(text string, _ bool) { got += text }, nil, nil, nil, nil)
	}()

	sendFrame(t, stream, errCh, textDeltaFrame("a"))
	sendFrame(t, stream, errCh, heartbeatFrame())
	sendFrame(t, stream, errCh, thinkingDeltaFrame("think"))
	sendFrame(t, stream, errCh, heartbeatFrame())
	sendFrame(t, stream, errCh, textDeltaFrame("b"))
	sendFrame(t, stream, errCh, turnEndedFrame())

	if err := waitProcessor(t, errCh, 2*time.Second); err != nil {
		t.Fatalf("healthy mix aborted: %v", err)
	}
	if got != "athinkb" && got != "ab" {
		// thinking may be delivered via onText(isThinking=true)
		if got == "" {
			t.Fatalf("expected text, got %q", got)
		}
	}
	snap := obsPtr.snapshot()
	if snap.Terminal != stallTerminalTurnEnded {
		t.Fatalf("terminal=%s, want turn_ended", snap.Terminal)
	}
	if snap.Crossed[50*time.Millisecond] {
		t.Fatal("healthy mix must not cross the 50ms shadow")
	}
}

func TestCursorSemanticWatchdogPausedWhileWaitingToolResult(t *testing.T) {
	withShortStallTimeout(t, 120*time.Millisecond)
	withShortToolBatchIdle(t, 20*time.Millisecond)
	withShadowThresholds(t, 40*time.Millisecond)
	obsPtr := attachStallObserver(t)

	stream := newFakeCursorStream()
	toolCh := make(chan []toolResultInfo, 1)
	var batch []pendingMcpExec
	gotBatch := make(chan struct{}, 1)
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil,
			func(execs []pendingMcpExec) {
				batch = append([]pendingMcpExec(nil), execs...)
				select {
				case gotBatch <- struct{}{}:
				default:
				}
			},
			toolCh, nil, nil)
	}()

	sendFrame(t, stream, errCh, framedParallelFixture(t, "064-recv.bin"))

	select {
	case <-gotBatch:
	case err := <-errCh:
		t.Fatalf("processor exited before tool wait: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("tool batch never finalized")
	}

	stopHB := make(chan struct{})
	go func() {
		for {
			select {
			case <-stopHB:
				return
			case stream.data <- heartbeatFrame():
			case <-time.After(30 * time.Millisecond):
			}
			select {
			case <-stopHB:
				return
			case <-time.After(25 * time.Millisecond):
			}
		}
	}()
	time.Sleep(300 * time.Millisecond) // > 2x semantic timeout
	close(stopHB)

	if len(batch) == 0 {
		t.Fatal("expected a pending tool call")
	}
	toolCh <- []toolResultInfo{{ToolCallId: batch[0].ToolCallId, Content: "ok"}}
	sendFrame(t, stream, errCh, textDeltaFrame("after"))
	sendFrame(t, stream, errCh, turnEndedFrame())

	if err := waitProcessor(t, errCh, 2*time.Second); err != nil {
		t.Fatalf("tool-wait must not trip watchdog: %v", err)
	}
	snap := obsPtr.snapshot()
	if snap.Terminal != stallTerminalTurnEnded {
		t.Fatalf("terminal=%s, want turn_ended", snap.Terminal)
	}
	if snap.ToolRoundCount < 1 {
		t.Fatalf("tool_round_count=%d, want >=1", snap.ToolRoundCount)
	}
	if snap.Crossed[40*time.Millisecond] && snap.GenerationState == stallStateWaitingTool {
		t.Fatal("shadow must not count WAITING_CLIENT_TOOL_RESULT idle as a would-abort")
	}
}

func TestCursorStallT3HeartbeatOnlyZombieAborts(t *testing.T) {
	withShortStallTimeout(t, 100*time.Millisecond)
	withShadowThresholds(t, 40*time.Millisecond)
	obsPtr := attachStallObserver(t)

	stream := newFakeCursorStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()
	sendFrame(t, stream, errCh, textDeltaFrame("hi"))
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case stream.data <- heartbeatFrame():
			case <-stop:
				return
			}
			select {
			case <-time.After(20 * time.Millisecond):
			case <-stop:
				return
			}
		}
	}()

	err := waitProcessor(t, errCh, 2*time.Second)
	if err == nil {
		t.Fatal("expected watchdog abort")
	}
	se, ok := err.(interface{ StatusCode() int })
	if !ok || se.StatusCode() != 504 {
		t.Fatalf("got %v, want 504", err)
	}
	snap := obsPtr.snapshot()
	if snap.Terminal != stallTerminalWatchdog {
		t.Fatalf("terminal=%s, want watchdog", snap.Terminal)
	}
	if !snap.Crossed[40*time.Millisecond] {
		t.Fatal("zombie must cross the 40ms shadow before abort")
	}
	if snap.ResumedAfter[40*time.Millisecond] {
		t.Fatal("heartbeat-only zombie must not record semantic resume")
	}
}

func TestCursorStallT4TransportDeadAborts(t *testing.T) {
	withShortStallTimeout(t, 80*time.Millisecond)
	obsPtr := attachStallObserver(t)
	stream := newFakeCursorStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()
	sendFrame(t, stream, errCh, textDeltaFrame("hi"))
	err := waitProcessor(t, errCh, 2*time.Second)
	if err == nil {
		t.Fatal("expected abort on silent stream")
	}
	snap := obsPtr.snapshot()
	if snap.Terminal != stallTerminalWatchdog {
		t.Fatalf("terminal=%s, want watchdog", snap.Terminal)
	}
}

func TestCursorStallT5ShadowFalseAbortThenResume(t *testing.T) {
	withShortStallTimeout(t, 250*time.Millisecond)
	withShadowThresholds(t, 50*time.Millisecond)
	obsPtr := attachStallObserver(t)

	stream := newFakeCursorStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil,
			func(string, bool) {}, nil, nil, nil, nil)
	}()
	sendFrame(t, stream, errCh, textDeltaFrame("start"))
	deadline := time.Now().Add(80 * time.Millisecond)
	for time.Now().Before(deadline) {
		sendFrame(t, stream, errCh, heartbeatFrame())
		time.Sleep(15 * time.Millisecond)
	}
	sendFrame(t, stream, errCh, textDeltaFrame("resume"))
	sendFrame(t, stream, errCh, turnEndedFrame())

	if err := waitProcessor(t, errCh, 2*time.Second); err != nil {
		t.Fatalf("resume before actual abort must succeed: %v", err)
	}
	snap := obsPtr.snapshot()
	if !snap.Crossed[50*time.Millisecond] {
		t.Fatal("expected shadow 50ms would-abort")
	}
	if !snap.ResumedAfter[50*time.Millisecond] {
		t.Fatal("expected semantic_resumed_after_50")
	}
	if snap.Terminal != stallTerminalTurnEnded {
		t.Fatalf("terminal=%s, want turn_ended", snap.Terminal)
	}
}
