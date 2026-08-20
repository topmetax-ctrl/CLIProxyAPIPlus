package executor

import (
	"context"
	"strings"
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

// thinkingDeltaFrame builds AgentServerMessage{ interaction_update{ thinking_delta{ text } } }.
func thinkingDeltaFrame(text string) []byte {
	inner := append([]byte{0x0a, byte(len(text))}, []byte(text)...)
	mid := append([]byte{0x22, byte(len(inner))}, inner...)
	outer := append([]byte{0x0a, byte(len(mid))}, mid...)
	return cursorproto.FrameConnectMessage(outer, 0)
}

func withShortTransportIdle(t *testing.T, d time.Duration) {
	t.Helper()
	prev := cursorTransportIdleTimeout
	cursorTransportIdleTimeout = d
	t.Cleanup(func() { cursorTransportIdleTimeout = prev })
}

func withShortSemanticIdle(t *testing.T, d time.Duration) {
	t.Helper()
	prev := cursorSemanticIdleWarning
	cursorSemanticIdleWarning = d
	t.Cleanup(func() { cursorSemanticIdleWarning = prev })
}

func withShortMaxDuration(t *testing.T, d time.Duration) {
	t.Helper()
	prev := cursorMaxStreamDuration
	cursorMaxStreamDuration = d
	t.Cleanup(func() { cursorMaxStreamDuration = prev })
}

func withShortHeartbeatInterval(t *testing.T, d time.Duration) {
	t.Helper()
	prev := cursorHeartbeatInterval
	cursorHeartbeatInterval = d
	t.Cleanup(func() { cursorHeartbeatInterval = prev })
}

func startUpstreamHeartbeatPump(stream *fakeCursorStream, stop <-chan struct{}) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
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
	return done
}

func waitCleanClose(t *testing.T, errCh <-chan error, stream *fakeCursorStream) {
	t.Helper()
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
}

func assertWatchdog504(t *testing.T, err error, reason string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected stall error, got nil")
	}
	se, ok := err.(interface{ StatusCode() int })
	if !ok || se.StatusCode() != 504 {
		t.Fatalf("expected 504 stall error, got %v", err)
	}
	scoped, ok := err.(interface{ IsRequestScoped() bool })
	if !ok || !scoped.IsRequestScoped() {
		t.Fatalf("stall 504 must be request-scoped so it does not cooldown the only credential, got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), reason) {
		t.Fatalf("expected reason %s, got %v", reason, err)
	}
}

func sendFrame(t *testing.T, stream *fakeCursorStream, errCh <-chan error, frame []byte) {
	t.Helper()
	select {
	case stream.data <- frame:
	case err := <-errCh:
		t.Fatalf("frame processor exited early: %v", err)
	}
}

// Screenshot 09:47 pattern: thinking + text then upstream heartbeats past the
// old 240s semantic threshold. Transport stays alive; no 504.
func TestCursorP0K1_XhighUpstreamHeartbeatParkDoesNotStall(t *testing.T) {
	withShortTransportIdle(t, 250*time.Millisecond)
	withShortSemanticIdle(t, 80*time.Millisecond)
	withShortMaxDuration(t, 0)

	stream := newFakeCursorStream()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()

	sendFrame(t, stream, errCh, thinkingDeltaFrame("think"))
	sendFrame(t, stream, errCh, textDeltaFrame("hello"))
	pumpDone := startUpstreamHeartbeatPump(stream, stop)

	select {
	case err := <-errCh:
		t.Fatalf("xhigh heartbeat park was killed: %v", err)
	case <-time.After(800 * time.Millisecond):
	}
	close(stop)
	<-pumpDone
	waitCleanClose(t, errCh, stream)
}

// Upstream stops ALL frames, including ServerMsgHeartbeat.
func TestCursorP0K1_SilentUpstreamIsTransportIdle(t *testing.T) {
	withShortTransportIdle(t, 250*time.Millisecond)
	withShortMaxDuration(t, 0)

	stream := newFakeCursorStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()

	select {
	case err := <-errCh:
		assertWatchdog504(t, err, cursorReasonFirstByteTimeout)
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not fire on silent stream")
	}
}

// Local ClientHeartbeat writes must not keep a silent upstream alive.
func TestCursorP0K1_LocalKeepaliveDoesNotResetTransportIdle(t *testing.T) {
	withShortTransportIdle(t, 250*time.Millisecond)
	withShortHeartbeatInterval(t, 20*time.Millisecond)
	withShortMaxDuration(t, 0)

	stream := newFakeCursorStream()
	stats := newCursorLivenessStats()
	ctx, cancel := context.WithCancel(withCursorLiveness(context.Background(), stats))
	defer cancel()
	go cursorH2Heartbeat(ctx, stream)

	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(ctx, stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()

	select {
	case err := <-errCh:
		assertWatchdog504(t, err, cursorReasonFirstByteTimeout)
		if stats.localKeepalive.Load() == 0 {
			t.Fatal("expected local ClientHeartbeat writes during the idle window")
		}
		if stats.upstreamHeartbeat.Load() != 0 {
			t.Fatalf("local keepalive was counted as upstream heartbeat: %d", stats.upstreamHeartbeat.Load())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("local keepalive incorrectly prevented transport idle abort")
	}
}

func TestCursorP0K1_SemanticIdleWarnsWithoutAbort(t *testing.T) {
	withShortTransportIdle(t, 2*time.Second)
	withShortSemanticIdle(t, 80*time.Millisecond)
	withShortMaxDuration(t, 0)

	warned := 0
	prev := cursorSemanticIdleHook
	cursorSemanticIdleHook = func(*cursorLivenessStats) {
		warned++
	}
	t.Cleanup(func() { cursorSemanticIdleHook = prev })

	stream := newFakeCursorStream()
	stop := make(chan struct{})
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()
	sendFrame(t, stream, errCh, textDeltaFrame("hello"))
	pumpDone := startUpstreamHeartbeatPump(stream, stop)

	deadline := time.After(2 * time.Second)
	for warned == 0 {
		select {
		case err := <-errCh:
			t.Fatalf("semantic idle aborted the stream: %v", err)
		case <-deadline:
			t.Fatal("semantic idle warning did not fire")
		case <-time.After(20 * time.Millisecond):
		}
	}
	select {
	case err := <-errCh:
		t.Fatalf("stream died after semantic idle warning: %v", err)
	case <-time.After(250 * time.Millisecond):
	}
	if warned != 1 {
		t.Fatalf("semantic idle warned %d times, want once per episode", warned)
	}
	close(stop)
	<-pumpDone
	waitCleanClose(t, errCh, stream)
}

func TestCursorP0K1_MaxDurationAbortsHeartbeatPark(t *testing.T) {
	withShortTransportIdle(t, 10*time.Second)
	withShortMaxDuration(t, 300*time.Millisecond)

	stream := newFakeCursorStream()
	stop := make(chan struct{})
	pumpDone := startUpstreamHeartbeatPump(stream, stop)
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()

	select {
	case err := <-errCh:
		assertWatchdog504(t, err, cursorReasonMaxDuration)
	case <-time.After(3 * time.Second):
		t.Fatal("max stream duration did not fire")
	}
	close(stop)
	<-pumpDone
}

func TestCursorStallWatchdogResetsOnProgress(t *testing.T) {
	withShortTransportIdle(t, 300*time.Millisecond)
	withShortMaxDuration(t, 0)

	stream := newFakeCursorStream()
	var got string
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil,
			func(text string, _ bool) { got += text }, nil, nil, nil, nil)
	}()

	for i := 0; i < 8; i++ {
		sendFrame(t, stream, errCh, textDeltaFrame("x"))
		time.Sleep(100 * time.Millisecond)
	}
	waitCleanClose(t, errCh, stream)
	if got != "xxxxxxxx" {
		t.Fatalf("expected 8 text deltas, got %q", got)
	}
}

func TestCursorP0K1_TransportIdleAfterProgressThenSilence(t *testing.T) {
	withShortTransportIdle(t, 250*time.Millisecond)
	withShortMaxDuration(t, 0)

	stream := newFakeCursorStream()
	errCh := make(chan error, 1)
	go func() {
		errCh <- processH2SessionFrames(context.Background(), stream, map[string][]byte{}, nil, nil, nil, nil, nil, nil)
	}()
	sendFrame(t, stream, errCh, textDeltaFrame("hello"))

	select {
	case err := <-errCh:
		assertWatchdog504(t, err, cursorReasonTransportIdle)
	case <-time.After(3 * time.Second):
		t.Fatal("watchdog did not fire after progress then silence")
	}
}
