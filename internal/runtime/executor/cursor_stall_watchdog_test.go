package executor

import (
	"context"
	"testing"
	"time"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
)

// heartbeatFrame is the wire form of a server heartbeat:
// AgentServerMessage{ interaction_update{ heartbeat(field 13) } }.
func heartbeatFrame() []byte {
	return cursorproto.FrameConnectMessage([]byte{0x0a, 0x02, 0x6a, 0x00}, 0)
}

// textDeltaFrame builds AgentServerMessage{ interaction_update{ text_delta{ text } } }.
func textDeltaFrame(text string) []byte {
	inner := append([]byte{0x0a, byte(len(text))}, []byte(text)...)
	mid := append([]byte{0x0a, byte(len(inner))}, inner...)
	outer := append([]byte{0x0a, byte(len(mid))}, mid...)
	return cursorproto.FrameConnectMessage(outer, 0)
}

func withShortStallTimeout(t *testing.T, d time.Duration) {
	t.Helper()
	prev := cursorNoProgressTimeout
	cursorNoProgressTimeout = d
	t.Cleanup(func() { cursorNoProgressTimeout = prev })
}

// A stream that only emits heartbeats must be failed by the stall watchdog
// with a 504 instead of hanging forever (observed upstream stall mode).
func TestCursorStallWatchdogFailsHeartbeatOnlyStream(t *testing.T) {
	withShortStallTimeout(t, 250*time.Millisecond)

	stream := newFakeCursorStream()
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
			case <-time.After(50 * time.Millisecond):
			case <-stop:
				return
			}
		}
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected stall error, got nil")
		}
		se, ok := err.(interface{ StatusCode() int })
		if !ok || se.StatusCode() != 504 {
			t.Fatalf("expected 504 stall error, got %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not fire on heartbeat-only stream")
	}
}

// Content-bearing messages must keep resetting the watchdog: a stream that
// emits text slower than the heartbeat cadence but faster than the timeout
// must not be killed.
func TestCursorStallWatchdogResetsOnProgress(t *testing.T) {
	withShortStallTimeout(t, 300*time.Millisecond)

	stream := newFakeCursorStream()
	var got string
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil,
			func(text string, _ bool) { got += text }, nil, nil, nil, nil)
	}()

	// 8 deltas at 100ms intervals: total 800ms > timeout, so this only
	// survives if each delta resets the watchdog.
	for i := 0; i < 8; i++ {
		select {
		case stream.data <- textDeltaFrame("x"):
		case err := <-errCh:
			t.Fatalf("frame processor exited early: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	stream.mu.Lock()
	stream.err = nil
	stream.mu.Unlock()
	close(stream.data)

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("expected clean close, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("frame processor did not exit after stream close")
	}
	if got != "xxxxxxxx" {
		t.Fatalf("expected 8 text deltas, got %q", got)
	}
}
