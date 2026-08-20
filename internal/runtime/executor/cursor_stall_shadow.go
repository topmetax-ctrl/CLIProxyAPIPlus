package executor

import (
	"sync"
	"sync/atomic"
	"time"

	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	log "github.com/sirupsen/logrus"
)

// Shadow thresholds never abort. They only record whether a given idle gap
// would have killed a generation under a shorter hard timeout.
var cursorShadowStallThresholds = []time.Duration{
	30 * time.Second,
	60 * time.Second,
	90 * time.Second,
	120 * time.Second,
	180 * time.Second,
}

// Tests replace this with millisecond-scale clocks. Nil means production.
var cursorShadowStallThresholdsForTest []time.Duration

const (
	stallStateActiveModel      = "ACTIVE_MODEL"
	stallStateWaitingTool      = "WAITING_CLIENT_TOOL_RESULT"
	stallStateAfterToolResult  = "ACTIVE_AFTER_TOOL_RESULT"
	stallTerminalWatchdog      = "watchdog"
	stallTerminalTurnEnded     = "turn_ended"
	stallTerminalClientCancel  = "client_cancel"
	stallTerminalUpstreamError = "upstream_error"
	stallTerminalStreamEnd     = "stream_end"
	stallTerminalToolBatch     = "tool_batch_complete"
)

type cursorStallMetrics struct {
	watchdogAbort atomic.Int64
	shadowCross   [5]atomic.Int64
	resumeAfter   [5]atomic.Int64
}

var cursorStallCounters cursorStallMetrics

func resetCursorStallCountersForTest() {
	cursorStallCounters.watchdogAbort.Store(0)
	for i := range cursorStallCounters.shadowCross {
		cursorStallCounters.shadowCross[i].Store(0)
		cursorStallCounters.resumeAfter[i].Store(0)
	}
}

func shadowStallThresholds() []time.Duration {
	if cursorShadowStallThresholdsForTest != nil {
		return cursorShadowStallThresholdsForTest
	}
	return cursorShadowStallThresholds
}

func shadowIndex(th time.Duration) int {
	for i, v := range cursorShadowStallThresholds {
		if v == th {
			return i
		}
	}
	return -1
}

// cursorStallObserver is observation-only. Heartbeats update the transport
// clock and never the semantic clock. Crossing a shadow threshold does not
// abort the stream.
type cursorStallObserver struct {
	mu sync.Mutex

	requestID  string
	model      string
	continuity string
	streamID   string

	startedAt               time.Time
	lastTransportAt         time.Time
	lastSemanticAt          time.Time
	lastSemanticKind        string
	heartbeatsSinceSemantic int
	hasText                 bool
	hasThinking             bool
	hasTool                 bool
	toolRoundCount          int
	generationState         string
	paused                  bool
	thresholds              []time.Duration
	crossed                 map[time.Duration]bool
	resumedAfter            map[time.Duration]bool
	unknownTypes            map[int]int
	terminal                string
}

type cursorStallSnapshot struct {
	GenerationState         string
	LastSemanticKind        string
	HeartbeatsSinceSemantic int
	HasText                 bool
	HasThinking             bool
	HasTool                 bool
	ToolRoundCount          int
	Paused                  bool
	Terminal                string
	Crossed                 map[time.Duration]bool
	ResumedAfter            map[time.Duration]bool
	UnknownTypes            map[int]int
}

func newCursorStallObserver(streamID, requestID, model, continuity string) *cursorStallObserver {
	now := time.Now()
	obs := &cursorStallObserver{
		requestID:        requestID,
		model:            model,
		continuity:       continuity,
		streamID:         streamID,
		startedAt:        now,
		lastTransportAt:  now,
		lastSemanticAt:   now,
		lastSemanticKind: "none",
		generationState:  stallStateActiveModel,
		thresholds:       shadowStallThresholds(),
		crossed:          make(map[time.Duration]bool),
		resumedAfter:     make(map[time.Duration]bool),
		unknownTypes:     make(map[int]int),
	}
	if cursorStallObserverHook != nil {
		cursorStallObserverHook(obs)
	}
	return obs
}

// Test hook: production leaves this nil.
var cursorStallObserverHook func(*cursorStallObserver)

func (o *cursorStallObserver) pauseForToolWait() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.paused = true
	o.generationState = stallStateWaitingTool
}

func (o *cursorStallObserver) resumeAfterToolWait() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	o.paused = false
	o.generationState = stallStateAfterToolResult
	o.toolRoundCount++
	o.lastSemanticAt = now
	o.lastTransportAt = now
	o.heartbeatsSinceSemantic = 0
	o.lastSemanticKind = "tool_result"
}

func (o *cursorStallObserver) noteHeartbeat() {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	o.lastTransportAt = now
	if o.paused {
		return
	}
	o.heartbeatsSinceSemantic++
	o.checkCrossingsLocked(now)
}

func (o *cursorStallObserver) noteProgress(msgType cursorproto.ServerMessageType) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	now := time.Now()
	if !o.paused {
		for _, th := range o.thresholds {
			if o.crossed[th] && !o.resumedAfter[th] {
				o.resumedAfter[th] = true
				if idx := shadowIndex(th); idx >= 0 {
					cursorStallCounters.resumeAfter[idx].Add(1)
				}
				log.WithFields(o.fieldsLocked(now)).WithFields(log.Fields{
					"event":     "cursor_semantic_resume_after_threshold",
					"threshold": th.String(),
				}).Info("cursor_semantic_resume_after_threshold")
			}
		}
	}
	if o.paused {
		o.lastTransportAt = now
		return
	}
	kind := cursorSemanticKind(msgType)
	switch msgType {
	case cursorproto.ServerMsgTextDelta:
		o.hasText = true
	case cursorproto.ServerMsgThinkingDelta, cursorproto.ServerMsgThinkingCompleted:
		o.hasThinking = true
	case cursorproto.ServerMsgExecMcpArgs:
		o.hasTool = true
	default:
		if len(kind) >= 5 && kind[:5] == "type_" {
			o.unknownTypes[int(msgType)]++
		}
	}
	o.lastTransportAt = now
	o.lastSemanticAt = now
	o.lastSemanticKind = kind
	o.heartbeatsSinceSemantic = 0
}

func (o *cursorStallObserver) finish(reason string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.terminal != "" {
		return
	}
	now := time.Now()
	if reason == stallTerminalWatchdog {
		o.checkCrossingsLocked(now)
		cursorStallCounters.watchdogAbort.Add(1)
	}
	o.terminal = reason
	log.WithFields(o.fieldsLocked(now)).WithFields(log.Fields{
		"event":    "cursor_stall_terminal",
		"terminal": reason,
	}).Info("cursor_stall_terminal")
}

func (o *cursorStallObserver) snapshot() cursorStallSnapshot {
	if o == nil {
		return cursorStallSnapshot{}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	crossed := make(map[time.Duration]bool, len(o.crossed))
	for k, v := range o.crossed {
		crossed[k] = v
	}
	resumed := make(map[time.Duration]bool, len(o.resumedAfter))
	for k, v := range o.resumedAfter {
		resumed[k] = v
	}
	unknown := make(map[int]int, len(o.unknownTypes))
	for k, v := range o.unknownTypes {
		unknown[k] = v
	}
	return cursorStallSnapshot{
		GenerationState:         o.generationState,
		LastSemanticKind:        o.lastSemanticKind,
		HeartbeatsSinceSemantic: o.heartbeatsSinceSemantic,
		HasText:                 o.hasText,
		HasThinking:             o.hasThinking,
		HasTool:                 o.hasTool,
		ToolRoundCount:          o.toolRoundCount,
		Paused:                  o.paused,
		Terminal:                o.terminal,
		Crossed:                 crossed,
		ResumedAfter:            resumed,
		UnknownTypes:            unknown,
	}
}

func (o *cursorStallObserver) checkCrossingsLocked(now time.Time) {
	if o.paused {
		return
	}
	gap := now.Sub(o.lastSemanticAt)
	for _, th := range o.thresholds {
		if gap < th || o.crossed[th] {
			continue
		}
		o.crossed[th] = true
		if idx := shadowIndex(th); idx >= 0 {
			cursorStallCounters.shadowCross[idx].Add(1)
		}
		log.WithFields(o.fieldsLocked(now)).WithFields(log.Fields{
			"event":        "cursor_shadow_threshold_cross",
			"threshold":    th.String(),
			"semantic_gap": gap.Round(time.Millisecond).String(),
		}).Info("cursor_shadow_threshold_cross")
	}
}

func (o *cursorStallObserver) fieldsLocked(now time.Time) log.Fields {
	gap := now.Sub(o.lastSemanticAt)
	return log.Fields{
		"request_id":                o.requestID,
		"model":                     o.model,
		"continuity":                o.continuity,
		"stream_id":                 o.streamID,
		"generation_state":          o.generationState,
		"last_semantic_kind":        o.lastSemanticKind,
		"heartbeats_since_semantic": o.heartbeatsSinceSemantic,
		"has_seen_text":             o.hasText,
		"has_seen_thinking":         o.hasThinking,
		"has_seen_tool":             o.hasTool,
		"tool_round_count":          o.toolRoundCount,
		"semantic_gap":              gap.Round(time.Millisecond).String(),
		"transport_age":             now.Sub(o.lastTransportAt).Round(time.Millisecond).String(),
	}
}
