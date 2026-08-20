package executor

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	log "github.com/sirupsen/logrus"
)

// Watchdog abort reasons. All are request-scoped local policy: they must not
// cooldown the Cursor credential.
const (
	cursorReasonFirstByteTimeout = "UPSTREAM_FIRST_BYTE_TIMEOUT"
	cursorReasonTransportIdle    = "UPSTREAM_TRANSPORT_IDLE"
	cursorReasonStreamStalled    = "UPSTREAM_STREAM_STALLED"
	cursorReasonMaxDuration      = "MAX_STREAM_DURATION_EXCEEDED"
)

type cursorLivenessCtxKey struct{}

// cursorLivenessStats separates Cursor upstream frames from local keepalives.
// Transport liveness is driven only by inbound H2 data (including
// ServerMsgHeartbeat). cursorH2Heartbeat writes ClientHeartbeat on the same
// stream; those must not reset the idle clock.
type cursorLivenessStats struct {
	lastUpstreamFrameAt   atomic.Int64
	lastSemanticEventAt   atomic.Int64
	lastDownstreamWriteAt atomic.Int64
	upstreamHeartbeat     atomic.Int64
	upstreamThinking      atomic.Int64
	upstreamText          atomic.Int64
	upstreamTool          atomic.Int64
	localKeepalive        atomic.Int64
	upstreamFrames        atomic.Int64
}

func newCursorLivenessStats() *cursorLivenessStats {
	return &cursorLivenessStats{}
}

func withCursorLiveness(ctx context.Context, stats *cursorLivenessStats) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if stats == nil {
		stats = newCursorLivenessStats()
	}
	return context.WithValue(ctx, cursorLivenessCtxKey{}, stats)
}

func cursorLivenessFrom(ctx context.Context) *cursorLivenessStats {
	if ctx == nil {
		return nil
	}
	stats, _ := ctx.Value(cursorLivenessCtxKey{}).(*cursorLivenessStats)
	return stats
}

func (s *cursorLivenessStats) noteUnix(slot *atomic.Int64) {
	if s == nil {
		return
	}
	slot.Store(time.Now().UnixNano())
}

func (s *cursorLivenessStats) noteUpstreamFrame() {
	if s == nil {
		return
	}
	s.upstreamFrames.Add(1)
	s.noteUnix(&s.lastUpstreamFrameAt)
}

func (s *cursorLivenessStats) noteUpstreamHeartbeat() {
	if s == nil {
		return
	}
	s.upstreamHeartbeat.Add(1)
}

func (s *cursorLivenessStats) noteSemanticThinking() {
	if s == nil {
		return
	}
	s.upstreamThinking.Add(1)
	s.noteUnix(&s.lastSemanticEventAt)
}

func (s *cursorLivenessStats) noteSemanticText() {
	if s == nil {
		return
	}
	s.upstreamText.Add(1)
	s.noteUnix(&s.lastSemanticEventAt)
}

func (s *cursorLivenessStats) noteSemanticTool() {
	if s == nil {
		return
	}
	s.upstreamTool.Add(1)
	s.noteUnix(&s.lastSemanticEventAt)
}

func (s *cursorLivenessStats) noteLocalKeepalive() {
	if s == nil {
		return
	}
	s.localKeepalive.Add(1)
}

func (s *cursorLivenessStats) noteDownstreamWrite() {
	if s == nil {
		return
	}
	s.noteUnix(&s.lastDownstreamWriteAt)
}

func (s *cursorLivenessStats) hadUpstreamFrame() bool {
	return s != nil && s.upstreamFrames.Load() > 0
}

func (s *cursorLivenessStats) logFields(streamID string) log.Fields {
	now := time.Now()
	fields := log.Fields{
		"stream_id":                streamID,
		"upstream_heartbeat":       int64(0),
		"upstream_thinking":        int64(0),
		"upstream_text":            int64(0),
		"upstream_tool":            int64(0),
		"local_keepalive":          int64(0),
		"last_upstream_frame_at":   "",
		"last_semantic_event_at":   "",
		"last_downstream_write_at": "",
	}
	if s == nil {
		return fields
	}
	fields["upstream_heartbeat"] = s.upstreamHeartbeat.Load()
	fields["upstream_thinking"] = s.upstreamThinking.Load()
	fields["upstream_text"] = s.upstreamText.Load()
	fields["upstream_tool"] = s.upstreamTool.Load()
	fields["local_keepalive"] = s.localKeepalive.Load()
	fields["last_upstream_frame_at"] = formatLivenessAge(now, s.lastUpstreamFrameAt.Load())
	fields["last_semantic_event_at"] = formatLivenessAge(now, s.lastSemanticEventAt.Load())
	fields["last_downstream_write_at"] = formatLivenessAge(now, s.lastDownstreamWriteAt.Load())
	return fields
}

func formatLivenessAge(now time.Time, unixNano int64) string {
	if unixNano == 0 {
		return "never"
	}
	return now.Sub(time.Unix(0, unixNano)).Round(time.Millisecond).String()
}

func durationSecondsEnv(keys ...string) (time.Duration, bool) {
	for _, key := range keys {
		if key == "" {
			continue
		}
		s := os.Getenv(key)
		if s == "" {
			continue
		}
		n, err := strconv.Atoi(s)
		if err != nil {
			continue
		}
		if n < 0 {
			return 0, true
		}
		if n == 0 {
			return 0, true
		}
		return time.Duration(n) * time.Second, true
	}
	return 0, false
}

func stopTimer(t *time.Timer) {
	if t == nil {
		return
	}
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
}

func cursorWatchdogErr(reason string, timeout time.Duration) error {
	return cursorStatusErr{
		code:          504,
		msg:           fmt.Sprintf("cursor: %s: no upstream frames within %s", reason, timeout),
		requestScoped: true,
	}
}

func cursorMaxDurationErr(limit time.Duration) error {
	return cursorStatusErr{
		code:          504,
		msg:           fmt.Sprintf("cursor: %s: stream exceeded %s", cursorReasonMaxDuration, limit),
		requestScoped: true,
	}
}

func applyCursorStreamTimeouts(cfg config.CursorConfig) {
	if cfg.TransportIdleTimeoutSeconds > 0 {
		cursorTransportIdleTimeout = time.Duration(cfg.TransportIdleTimeoutSeconds) * time.Second
	}
	if cfg.SemanticIdleWarningSeconds > 0 {
		cursorSemanticIdleWarning = time.Duration(cfg.SemanticIdleWarningSeconds) * time.Second
	}
	if cfg.MaxStreamDurationSeconds > 0 {
		cursorMaxStreamDuration = time.Duration(cfg.MaxStreamDurationSeconds) * time.Second
	} else if cfg.MaxStreamDurationSeconds < 0 {
		cursorMaxStreamDuration = 0
	}
}

func (e *CursorExecutor) markStreamGenerationsFinished(conversationID string, stream cursorStream) {
	if e == nil || conversationID == "" {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	sessions, _ := e.collectConversationSessions(conversationID)
	for _, session := range sessions {
		if stream != nil && session.stream == stream {
			session.finished = true
		}
	}
}

// Test hook: fired when semantic idle warns. Production leaves this nil.
var cursorSemanticIdleHook func(stats *cursorLivenessStats)
