// Cursor parked-session tracing (P0-A).
//
// These helpers log hashed identifiers only and do not change park, restore,
// or tool-result match behavior. A replace with old_pending_count > 0 is an
// invariant violation for the current one-slot map; it is not by itself proof
// that a historical incident was caused by that overwrite.
package executor

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

// Instrumentation-only counters. They do not change park/restore/match behavior.
var (
	cursorSessionReplaceWithPendingTotal           atomic.Int64
	cursorSessionRestoreMultiConsumerTotal         atomic.Int64
	cursorToolResultMixedGenerationTotal           atomic.Int64
	cursorSessionGenerationLookupMissTotal         atomic.Int64
	cursorSessionGenerationExpiredWithPendingTotal atomic.Int64
	cursorSessionForcedExpireWithPendingTotal      atomic.Int64
	cursorGenerationDuplicateResultTotal           atomic.Int64
	cursorGenerationParkTotal                      atomic.Int64
	cursorGenerationResolveTotal                   atomic.Int64
	cursorSessionConsumedGCTotal                   atomic.Int64
	cursorSessionConsumedIndexEntries              atomic.Int64
	cursorToolResultClaimTotal                     atomic.Int64
	cursorToolResultReplayTotal                    atomic.Int64
	cursorToolResultCommitTotal                    atomic.Int64
	cursorToolResultInFlightConflictTotal          atomic.Int64
	cursorToolResultFinalRejectTotal               atomic.Int64
	cursorToolResultStaleClaimRecoveredTotal       atomic.Int64
)

// CursorSessionReplaceWithPendingTotal reports how many times a parked
// generation with unresolved pending tool calls was overwritten or evicted.
func CursorSessionReplaceWithPendingTotal() int64 {
	return cursorSessionReplaceWithPendingTotal.Load()
}

// CursorSessionRestoreMultiConsumerTotal reports generations restored by more
// than one HTTP request (retries after a failed match count).
func CursorSessionRestoreMultiConsumerTotal() int64 {
	return cursorSessionRestoreMultiConsumerTotal.Load()
}

func CursorToolResultMixedGenerationTotal() int64 {
	return cursorToolResultMixedGenerationTotal.Load()
}

func CursorSessionGenerationLookupMissTotal() int64 {
	return cursorSessionGenerationLookupMissTotal.Load()
}

func CursorSessionGenerationExpiredWithPendingTotal() int64 {
	return cursorSessionGenerationExpiredWithPendingTotal.Load()
}

func CursorSessionForcedExpireWithPendingTotal() int64 {
	return cursorSessionForcedExpireWithPendingTotal.Load()
}

func CursorGenerationDuplicateResultTotal() int64 {
	return cursorGenerationDuplicateResultTotal.Load()
}

func CursorSessionConsumedGCTotal() int64 {
	return cursorSessionConsumedGCTotal.Load()
}

func CursorSessionConsumedIndexEntries() int64 {
	return cursorSessionConsumedIndexEntries.Load()
}

func CursorToolResultClaimTotal() int64  { return cursorToolResultClaimTotal.Load() }
func CursorToolResultReplayTotal() int64 { return cursorToolResultReplayTotal.Load() }
func CursorToolResultCommitTotal() int64 { return cursorToolResultCommitTotal.Load() }
func CursorToolResultInFlightConflictTotal() int64 {
	return cursorToolResultInFlightConflictTotal.Load()
}
func CursorToolResultFinalRejectTotal() int64 { return cursorToolResultFinalRejectTotal.Load() }
func CursorToolResultStaleClaimRecoveredTotal() int64 {
	return cursorToolResultStaleClaimRecoveredTotal.Load()
}

type cursorSessionReplaceEvent struct {
	key    string
	old    *cursorSessionTraceSnapshot
	reason string
}

type cursorSessionTraceSnapshot struct {
	generationID       string
	parentGenerationID string
	sourceRequestID    string
	pendingCount       int
	pendingSetHash     string
	pendingIDHashes    []string
	restoreCount       uint64
	createdAt          time.Time
	consumedAt         time.Time
}

func snapshotCursorSession(session *cursorSession) *cursorSessionTraceSnapshot {
	if session == nil {
		return nil
	}
	ids := pendingToolCallIDs(session.pending)
	return &cursorSessionTraceSnapshot{
		generationID:       session.generationID,
		parentGenerationID: session.parentGenerationID,
		sourceRequestID:    session.sourceRequestID,
		pendingCount:       len(ids),
		pendingSetHash:     cursorToolIDSetHash(ids),
		pendingIDHashes:    cursorToolIDHashes(ids),
		restoreCount:       session.restoreCount,
		createdAt:          session.createdAt,
		consumedAt:         session.consumedAt,
	}
}

func pendingToolCallIDs(pending []pendingMcpExec) []string {
	ids := make([]string, 0, len(pending))
	for _, item := range pending {
		if item.ToolCallId != "" {
			ids = append(ids, item.ToolCallId)
		}
	}
	return ids
}

func incomingToolCallIDs(results []toolResultInfo) []string {
	ids := make([]string, 0, len(results))
	for _, item := range results {
		if item.ToolCallId != "" {
			ids = append(ids, item.ToolCallId)
		}
	}
	return ids
}

func hashCursorSessionKey(sessionKey string) string {
	sum := sha256.Sum256([]byte(sessionKey))
	return hex.EncodeToString(sum[:8])
}

func cursorToolIDHash(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:6])
}

func cursorToolIDHashes(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, cursorToolIDHash(id))
	}
	return out
}

func cursorToolIDSetHash(ids []string) string {
	uniq := uniqueSortedStrings(ids)
	sum := sha256.Sum256([]byte(strings.Join(uniq, "\n")))
	return hex.EncodeToString(sum[:8])
}

func uniqueSortedStrings(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func cursorToolIDIntersectionCount(pending, incoming []string) int {
	set := make(map[string]struct{}, len(pending))
	for _, id := range pending {
		if id != "" {
			set[id] = struct{}{}
		}
	}
	matched := 0
	seen := make(map[string]struct{})
	for _, id := range incoming {
		if id == "" {
			continue
		}
		if _, ok := set[id]; !ok {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		matched++
	}
	return matched
}

func logCursorSessionPark(sessionKey string, snap *cursorSessionTraceSnapshot, replace bool, previousGenerationID string) {
	if snap == nil {
		return
	}
	fields := log.Fields{
		"event":                  "cursor_session_park",
		"session_key_hash":       hashCursorSessionKey(sessionKey),
		"generation_id":          snap.generationID,
		"source_request_id":      snap.sourceRequestID,
		"pending_count":          snap.pendingCount,
		"pending_set_hash":       snap.pendingSetHash,
		"created_at":             snap.createdAt.UTC().Format(time.RFC3339Nano),
		"replace":                replace,
		"previous_generation_id": previousGenerationID,
		"parent_generation_id":   snap.parentGenerationID,
	}
	log.WithFields(fields).Debug("cursor session park")
}

func logCursorSessionReplace(sessionKey, reason string, oldSnap, newSnap *cursorSessionTraceSnapshot) {
	fields := log.Fields{
		"event":            "cursor_session_replace",
		"session_key_hash": hashCursorSessionKey(sessionKey),
		"reason":           reason,
	}
	if oldSnap != nil {
		fields["old_generation_id"] = oldSnap.generationID
		fields["old_request_id"] = oldSnap.sourceRequestID
		fields["old_pending_count"] = oldSnap.pendingCount
		fields["old_pending_hash"] = oldSnap.pendingSetHash
		fields["old_pending_id_hashes"] = oldSnap.pendingIDHashes
	}
	if newSnap != nil {
		fields["new_generation_id"] = newSnap.generationID
		fields["new_request_id"] = newSnap.sourceRequestID
		fields["new_pending_count"] = newSnap.pendingCount
		fields["new_pending_hash"] = newSnap.pendingSetHash
		fields["new_pending_id_hashes"] = newSnap.pendingIDHashes
	}
	logger := log.WithFields(fields)
	if oldSnap != nil && oldSnap.pendingCount > 0 {
		cursorSessionReplaceWithPendingTotal.Add(1)
		logger.Warn("cursor session replaced while previous generation still had pending tool calls")
		return
	}
	logger.Debug("cursor session replace")
}

func logCursorSessionRestore(sessionKey, consumerRequestID string, snap *cursorSessionTraceSnapshot) {
	if snap == nil {
		return
	}
	fields := log.Fields{
		"event":               "cursor_session_restore",
		"session_key_hash":    hashCursorSessionKey(sessionKey),
		"consumer_request_id": consumerRequestID,
		"generation_id":       snap.generationID,
		"source_request_id":   snap.sourceRequestID,
		"pending_count":       snap.pendingCount,
		"pending_set_hash":    snap.pendingSetHash,
		"restore_count":       snap.restoreCount,
	}
	if !snap.consumedAt.IsZero() {
		fields["consumed_at"] = snap.consumedAt.UTC().Format(time.RFC3339Nano)
	}
	logger := log.WithFields(fields)
	if snap.restoreCount > 1 {
		logger.Warn("cursor session restored by more than one request")
		return
	}
	logger.Debug("cursor session restore")
}

func logCursorToolResultMatch(sessionKey, consumerRequestID string, snap *cursorSessionTraceSnapshot, pending, incoming []string) {
	generationID := ""
	restoreCount := uint64(0)
	sourceRequestID := ""
	if snap != nil {
		generationID = snap.generationID
		restoreCount = snap.restoreCount
		sourceRequestID = snap.sourceRequestID
	}
	intersection := cursorToolIDIntersectionCount(pending, incoming)
	fields := log.Fields{
		"event":               "cursor_tool_result_match",
		"session_key_hash":    hashCursorSessionKey(sessionKey),
		"generation_id":       generationID,
		"source_request_id":   sourceRequestID,
		"consumer_request_id": consumerRequestID,
		"incoming_count":      len(incoming),
		"incoming_set_hash":   cursorToolIDSetHash(incoming),
		"incoming_id_hashes":  cursorToolIDHashes(incoming),
		"pending_count":       len(pending),
		"pending_set_hash":    cursorToolIDSetHash(pending),
		"pending_id_hashes":   cursorToolIDHashes(pending),
		"intersection_count":  intersection,
		"restore_count":       restoreCount,
	}
	if snap != nil && !snap.consumedAt.IsZero() {
		fields["consumed_at"] = snap.consumedAt.UTC().Format(time.RFC3339Nano)
	}
	logger := log.WithFields(fields)
	if intersection == 0 {
		logger.Warn("cursor tool results do not intersect parked pending IDs")
		return
	}
	logger.Debug("cursor tool result match")
}
