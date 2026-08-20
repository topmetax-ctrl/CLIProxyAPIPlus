package executor

import internusage "github.com/router-for-me/CLIProxyAPI/v7/internal/usage"

// cursorUsageMetricSnapshot is the executor-facing view of Cursor coverage
// counters. Production exposure is via management /usage as cursor_usage.
type cursorUsageMetricSnapshot = internusage.CursorCoverageSnapshot

func recordCursorUsageSettlement(usage *cursorTokenUsage, class, reason string) {
	ev := internusage.CursorCoverageEvent{
		Source:           "payload_estimate",
		Class:            class,
		Reason:           reason,
		NoTurnEndedClass: cursorClassNoTurnEndedExpected,
		RealFailureClass: cursorClassRealFailure,
	}
	if usage != nil {
		usage.mu.Lock()
		ev.Source = usage.sourceLocked()
		if usage.terminalSeen && usage.terminal.HasCacheRead {
			ev.CacheKnown = true
			ev.HasCacheRead = true
			ev.CacheReadTokens = usage.terminal.CacheReadTokens
		}
		if usage.terminalSeen && usage.terminal.HasCacheWrite {
			ev.CacheKnown = true
			ev.HasCacheWrite = true
			ev.CacheWriteTokens = usage.terminal.CacheWriteTokens
		}
		ev.ProtocolAnomaly = usage.cacheReadExceedsInputLocked()
		usage.mu.Unlock()
	}
	internusage.RecordCursorCoverage(ev)
}

// CursorUsageMetricsSnapshot returns a copy of process-local coverage counters.
func CursorUsageMetricsSnapshot() cursorUsageMetricSnapshot {
	return internusage.CursorCoverageSnapshotNow()
}
