package usage

import "sync/atomic"

// CursorCoverageSnapshot is process-local Cursor terminal usage coverage.
// Labels stay low-cardinality (source/reason enums only).
type CursorCoverageSnapshot struct {
	TurnEnded           int64 `json:"cursor_usage_source_turn_ended"`
	TokenDelta          int64 `json:"cursor_usage_source_token_delta"`
	Estimate            int64 `json:"cursor_usage_source_estimate"`
	EOFWithoutTurnEnded int64 `json:"cursor_turnended_missing_eof"`
	ToolBoundary        int64 `json:"cursor_turnended_missing_tool_boundary"`
	Cancel              int64 `json:"cursor_turnended_missing_cancel"`
	Error               int64 `json:"cursor_turnended_missing_error"`
	CacheObservable     int64 `json:"cursor_cache_usage_observable_total"`
	CacheUnobservable   int64 `json:"cursor_cache_usage_unobservable_total"`
	CacheReadTokens     int64 `json:"cursor_cache_read_tokens_total"`
	CacheWriteTokens    int64 `json:"cursor_cache_write_tokens_total"`
	ProtocolAnomaly     int64 `json:"cursor_usage_protocol_anomaly_total"`
}

var (
	cursorMetricTurnEnded           atomic.Int64
	cursorMetricTokenDelta          atomic.Int64
	cursorMetricEstimate            atomic.Int64
	cursorMetricEOFWithoutTurnEnded atomic.Int64
	cursorMetricToolBoundary        atomic.Int64
	cursorMetricCancel              atomic.Int64
	cursorMetricError               atomic.Int64
	cursorMetricCacheObservable     atomic.Int64
	cursorMetricCacheUnobservable   atomic.Int64
	cursorMetricCacheReadTokens     atomic.Int64
	cursorMetricCacheWriteTokens    atomic.Int64
	cursorMetricProtocolAnomaly     atomic.Int64
)

// CursorCoverageEvent is one Cursor usage settlement.
type CursorCoverageEvent struct {
	Source           string
	Class            string
	Reason           string
	CacheKnown       bool
	CacheReadTokens  int64
	CacheWriteTokens int64
	HasCacheRead     bool
	HasCacheWrite    bool
	ProtocolAnomaly  bool
	NoTurnEndedClass string
	RealFailureClass string
}

// RecordCursorCoverage increments low-cardinality Cursor terminal coverage counters.
func RecordCursorCoverage(ev CursorCoverageEvent) {
	if ev.HasCacheRead {
		cursorMetricCacheReadTokens.Add(ev.CacheReadTokens)
	}
	if ev.HasCacheWrite {
		cursorMetricCacheWriteTokens.Add(ev.CacheWriteTokens)
	}
	if ev.ProtocolAnomaly {
		cursorMetricProtocolAnomaly.Add(1)
	}
	if ev.CacheKnown {
		cursorMetricCacheObservable.Add(1)
	} else {
		cursorMetricCacheUnobservable.Add(1)
	}
	switch ev.Source {
	case "cursor_turn_ended":
		cursorMetricTurnEnded.Add(1)
	case "token_delta":
		cursorMetricTokenDelta.Add(1)
	default:
		cursorMetricEstimate.Add(1)
	}
	switch {
	case ev.Class == ev.NoTurnEndedClass || ev.Reason == "tool_call_boundary_waiting_for_resume":
		cursorMetricToolBoundary.Add(1)
	case ev.Reason == "eof_without_turn_ended" || ev.Reason == "stop_without_turn_ended":
		cursorMetricEOFWithoutTurnEnded.Add(1)
	case ev.Reason == "cancelled":
		cursorMetricCancel.Add(1)
	case ev.Class == ev.RealFailureClass:
		cursorMetricError.Add(1)
	}
}

// CursorCoverageSnapshotNow returns a copy of process-local coverage counters.
func CursorCoverageSnapshotNow() CursorCoverageSnapshot {
	return CursorCoverageSnapshot{
		TurnEnded:           cursorMetricTurnEnded.Load(),
		TokenDelta:          cursorMetricTokenDelta.Load(),
		Estimate:            cursorMetricEstimate.Load(),
		EOFWithoutTurnEnded: cursorMetricEOFWithoutTurnEnded.Load(),
		ToolBoundary:        cursorMetricToolBoundary.Load(),
		Cancel:              cursorMetricCancel.Load(),
		Error:               cursorMetricError.Load(),
		CacheObservable:     cursorMetricCacheObservable.Load(),
		CacheUnobservable:   cursorMetricCacheUnobservable.Load(),
		CacheReadTokens:     cursorMetricCacheReadTokens.Load(),
		CacheWriteTokens:    cursorMetricCacheWriteTokens.Load(),
		ProtocolAnomaly:     cursorMetricProtocolAnomaly.Load(),
	}
}
