package executor

import (
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	log "github.com/sirupsen/logrus"
)

type generationState int

const (
	generationParked generationState = iota
	generationPartiallyConsumed
	generationConsumed
	generationStalePending
	generationExpired
)

func (s generationState) String() string {
	switch s {
	case generationParked:
		return "PARKED"
	case generationPartiallyConsumed:
		return "PARTIALLY_CONSUMED"
	case generationConsumed:
		return "CONSUMED"
	case generationStalePending:
		return "STALE_PENDING"
	case generationExpired:
		return "EXPIRED"
	default:
		return fmt.Sprintf("generationState(%d)", int(s))
	}
}

type conversationState struct {
	generations  map[string]*cursorSession
	pendingIndex map[string]string
	resultIndex  map[string]*toolResultRecord
}

func (e *CursorExecutor) ensureConversationLocked(sessionKey string) *conversationState {
	state := e.conversations[sessionKey]
	if state != nil {
		return state
	}
	state = &conversationState{
		generations:  make(map[string]*cursorSession),
		pendingIndex: make(map[string]string),
		resultIndex:  make(map[string]*toolResultRecord),
	}
	e.conversations[sessionKey] = state
	return state
}

func (e *CursorExecutor) canParkLocked(conversationID string, owner *cursorStateOwner) bool {
	current := e.stateOwners[conversationID]
	if current == owner {
		return true
	}
	// Conversation is still live under another generation. Sibling parks and
	// mismatch reparks must succeed; only a retired conversation rejects writes.
	return current != nil
}

func (e *CursorExecutor) parkGeneration(conversationID, sessionKey string, owner *cursorStateOwner, session *cursorSession) bool {
	e.mu.Lock()
	if !e.canParkLocked(conversationID, owner) {
		e.mu.Unlock()
		return false
	}
	if session.generationID == "" {
		session.generationID = uuid.New().String()
	}
	session.conversationID = conversationID
	session.owner = owner
	if session.createdAt.IsZero() {
		session.createdAt = time.Now()
	}
	session.updatedAt = time.Now()
	if session.state == generationParked && len(session.pending) > 0 && !session.consumedAt.IsZero() {
		session.state = generationPartiallyConsumed
	}
	state := e.ensureConversationLocked(sessionKey)
	for _, item := range session.pending {
		if item.ToolCallId == "" {
			continue
		}
		if prev, exists := state.pendingIndex[item.ToolCallId]; exists && prev != session.generationID {
			log.WithFields(log.Fields{
				"event":            "cursor_session_duplicate_tool_id",
				"session_key_hash": hashCursorSessionKey(sessionKey),
				"generation_id":    session.generationID,
			}).Warn("cursor pending tool ID already belongs to another generation")
			e.mu.Unlock()
			return false
		}
		if rec, exists := state.resultIndex[item.ToolCallId]; exists && rec.generationID != session.generationID {
			log.WithFields(log.Fields{
				"event":            "cursor_session_duplicate_consumed_tool_id",
				"session_key_hash": hashCursorSessionKey(sessionKey),
				"generation_id":    session.generationID,
			}).Warn("cursor pending tool ID still has a result record from another generation")
			e.mu.Unlock()
			return false
		}
		state.pendingIndex[item.ToolCallId] = session.generationID
	}
	state.generations[session.generationID] = session
	cursorGenerationParkTotal.Add(1)
	snap := snapshotCursorSession(session)
	e.mu.Unlock()
	logCursorSessionPark(sessionKey, snap, false, "")
	return true
}

func (e *CursorExecutor) reparkGeneration(conversationID, sessionKey string, owner *cursorStateOwner, session *cursorSession) bool {
	return e.parkGeneration(conversationID, sessionKey, owner, session)
}

func (e *CursorExecutor) resolveGenerationForToolResults(sessionKey string, incoming []string) (*cursorSession, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.resolveGenerationForToolResultsLocked(sessionKey, incoming)
}

func (e *CursorExecutor) resolveGenerationForToolResultsLocked(sessionKey string, incoming []string) (*cursorSession, error) {
	return e.inspectToolResultsLocked(sessionKey, incoming)
}

func (e *CursorExecutor) noteGenerationRestore(session *cursorSession) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if session == nil {
		return
	}
	session.restoreCount++
	if session.restoreCount > 1 {
		cursorSessionRestoreMultiConsumerTotal.Add(1)
	}
}

func (e *CursorExecutor) consumeToolResults(sessionKey string, session *cursorSession, incoming []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.consumeToolResultsLocked(sessionKey, session, incoming)
}

func (e *CursorExecutor) consumeToolResultsLocked(sessionKey string, session *cursorSession, incoming []string) {
	_, _ = e.claimToolResultsLocked(sessionKey, "", incoming)
	_ = session
}

func (e *CursorExecutor) restoreConsumedToolResults(sessionKey string, session *cursorSession, previous []pendingMcpExec) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if session == nil {
		return
	}
	state := e.conversations[sessionKey]
	if state == nil {
		return
	}
	session.pending = append([]pendingMcpExec(nil), previous...)
	session.state = generationParked
	session.consumedAt = time.Time{}
	session.updatedAt = time.Now()
	for _, item := range session.pending {
		if item.ToolCallId == "" {
			continue
		}
		delete(state.resultIndex, item.ToolCallId)
		state.pendingIndex[item.ToolCallId] = session.generationID
	}
	state.generations[session.generationID] = session
	e.refreshConsumedIndexGaugeLocked()
}

func (e *CursorExecutor) removeGenerationLocked(state *conversationState, session *cursorSession) {
	if state == nil || session == nil {
		return
	}
	for _, item := range session.pending {
		if state.pendingIndex[item.ToolCallId] == session.generationID {
			delete(state.pendingIndex, item.ToolCallId)
		}
	}
	delete(state.generations, session.generationID)
}

func (e *CursorExecutor) collectConversationSessions(conversationID string) (retired []*cursorSession, keys []string) {
	suffix := ":" + conversationID
	for key, state := range e.conversations {
		if !strings.HasSuffix(key, suffix) {
			continue
		}
		for _, session := range state.generations {
			retired = append(retired, session)
		}
		keys = append(keys, key)
	}
	return retired, keys
}

func (e *CursorExecutor) hasParkedSession(sessionKey string, session *cursorSession) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil || session == nil {
		return false
	}
	if session.generationID != "" {
		return state.generations[session.generationID] == session
	}
	for _, candidate := range state.generations {
		if candidate == session {
			return true
		}
	}
	return false
}

func (e *CursorExecutor) collectParkedPending() []pendingMcpExec {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []pendingMcpExec
	for _, state := range e.conversations {
		for _, session := range state.generations {
			out = append(out, session.pending...)
		}
	}
	return out
}

func (e *CursorExecutor) parkedGenerationCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, state := range e.conversations {
		n += len(state.generations)
	}
	return n
}

func (e *CursorExecutor) seedParkedSession(sessionKey string, session *cursorSession) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if session.generationID == "" {
		session.generationID = uuid.New().String()
	}
	state := e.ensureConversationLocked(sessionKey)
	state.generations[session.generationID] = session
	for _, item := range session.pending {
		if item.ToolCallId != "" {
			state.pendingIndex[item.ToolCallId] = session.generationID
		}
	}
}

func (e *CursorExecutor) gcConsumedIndexLocked(state *conversationState, now time.Time) {
	if state == nil {
		return
	}
	e.recoverStaleClaimsLocked(state, now)
	for toolID, rec := range state.resultIndex {
		if rec == nil {
			delete(state.resultIndex, toolID)
			continue
		}
		if rec.state != resultCommitted && rec.state != resultFinalRejected {
			continue
		}
		anchor := rec.lastTransitionAt
		if anchor.IsZero() {
			anchor = rec.claimedAt
		}
		if now.Sub(anchor) > cursorConsumedIndexTTL {
			delete(state.resultIndex, toolID)
			cursorSessionConsumedGCTotal.Add(1)
		}
	}
}

func (e *CursorExecutor) refreshConsumedIndexGaugeLocked() {
	n := 0
	for _, state := range e.conversations {
		n += len(state.resultIndex)
	}
	cursorSessionConsumedIndexEntries.Store(int64(n))
}

func (e *CursorExecutor) conversationInvariantErrorLocked() error {
	for key, state := range e.conversations {
		if state == nil {
			continue
		}
		owned := make(map[string]string, len(state.pendingIndex))
		for genID, session := range state.generations {
			if session == nil {
				return fmt.Errorf("%s: nil generation %s", key, genID)
			}
			if session.generationID != genID {
				return fmt.Errorf("%s: generation map key %s != session %s", key, genID, session.generationID)
			}
			pendingIDs := pendingToolCallIDs(session.pending)
			if session.state == generationConsumed && len(pendingIDs) != 0 {
				return fmt.Errorf("%s: consumed generation %s still has pending %v", key, genID, pendingIDs)
			}
			for _, toolID := range pendingIDs {
				if indexed := state.pendingIndex[toolID]; indexed != genID {
					return fmt.Errorf("%s: pending tool %s owned by %s, index=%s", key, toolID, genID, indexed)
				}
				if rec := state.resultIndex[toolID]; rec != nil {
					return fmt.Errorf("%s: tool %s is both pending and %s", key, toolID, rec.state)
				}
				if prev, exists := owned[toolID]; exists && prev != genID {
					return fmt.Errorf("%s: tool %s owned by both %s and %s", key, toolID, prev, genID)
				}
				owned[toolID] = genID
			}
		}
		for toolID, genID := range state.pendingIndex {
			if owned[toolID] != genID {
				return fmt.Errorf("%s: pendingIndex tool %s -> %s missing from generation pending", key, toolID, genID)
			}
			if state.generations[genID] == nil {
				return fmt.Errorf("%s: pendingIndex tool %s points at missing generation %s", key, toolID, genID)
			}
			if rec := state.resultIndex[toolID]; rec != nil {
				return fmt.Errorf("%s: tool %s is both pendingIndex and %s", key, toolID, rec.state)
			}
		}
	}
	return nil
}
