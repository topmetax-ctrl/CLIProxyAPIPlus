package executor

import (
	"time"

	log "github.com/sirupsen/logrus"
)

func (e *CursorExecutor) livePendingToolIDsLocked(sessionKey string) []string {
	state := e.conversations[sessionKey]
	if state == nil {
		return nil
	}
	ids := make([]string, 0, len(state.pendingIndex))
	for toolID := range state.pendingIndex {
		ids = append(ids, toolID)
	}
	return ids
}

func (e *CursorExecutor) backdateGenerationLocked(generationID string, age time.Duration) {
	ts := time.Now().Add(-age)
	for _, state := range e.conversations {
		if session := state.generations[generationID]; session != nil {
			session.createdAt = ts
			session.updatedAt = ts
			return
		}
	}
}

func (e *CursorExecutor) expireStaleSessionsLocked(expired *[]cursorSessionReplaceEvent) {
	now := time.Now()
	for key, state := range e.conversations {
		for _, session := range state.generations {
			age := now.Sub(session.createdAt)
			idle := age
			if !session.updatedAt.IsZero() {
				idle = now.Sub(session.updatedAt)
			}
			pendingCount := len(pendingToolCallIDs(session.pending))
			if pendingCount == 0 {
				if idle > cursorSessionTTL || age > cursorSessionHardTTL {
					*expired = append(*expired, cursorSessionReplaceEvent{key: key, old: snapshotCursorSession(session), reason: "ttl_expire"})
					if session.cancel != nil {
						session.cancel()
					}
					e.removeGenerationLocked(state, session)
				}
				continue
			}
			if age > cursorSessionHardTTL {
				session.state = generationExpired
				cursorSessionForcedExpireWithPendingTotal.Add(1)
				log.WithFields(log.Fields{
					"event":            "cursor_session_forced_expire_with_pending",
					"session_key_hash": hashCursorSessionKey(key),
					"generation_id":    session.generationID,
					"pending_count":    pendingCount,
				}).Warn("cursor generation exceeded hard TTL with pending tools; forcing expiry")
				*expired = append(*expired, cursorSessionReplaceEvent{key: key, old: snapshotCursorSession(session), reason: "hard_ttl_expire"})
				if session.cancel != nil {
					session.cancel()
				}
				e.removeGenerationLocked(state, session)
				continue
			}
			if idle > cursorSessionTTL {
				if session.state != generationStalePending {
					session.state = generationStalePending
				}
				if !session.expiryWarned {
					session.expiryWarned = true
					cursorSessionGenerationExpiredWithPendingTotal.Add(1)
					log.WithFields(log.Fields{
						"event":            "cursor_session_generation_expired_with_pending",
						"session_key_hash": hashCursorSessionKey(key),
						"generation_id":    session.generationID,
						"pending_count":    pendingCount,
					}).Warn("cursor generation exceeded soft TTL but still has pending tools")
				}
			}
		}
		e.gcConsumedIndexLocked(state, now)
		if len(state.generations) == 0 && len(state.pendingIndex) == 0 && len(state.consumedIndex) == 0 {
			delete(e.conversations, key)
		}
	}
	e.refreshConsumedIndexGaugeLocked()
}

// livePendingToolIDs returns still-unresolved tool IDs for a conversation key.
// Tests use this invariant; it must not depend on "latest generation only".
func (e *CursorExecutor) livePendingToolIDs(sessionKey string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.livePendingToolIDsLocked(sessionKey)
}

func (e *CursorExecutor) backdateGeneration(generationID string, age time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.backdateGenerationLocked(generationID, age)
}

func (e *CursorExecutor) consumedToolIDs(sessionKey string) []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return nil
	}
	ids := make([]string, 0, len(state.consumedIndex))
	for toolID := range state.consumedIndex {
		ids = append(ids, toolID)
	}
	return ids
}

func (e *CursorExecutor) generationStateOf(generationID string) (generationState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, state := range e.conversations {
		if session := state.generations[generationID]; session != nil {
			return session.state, true
		}
	}
	return 0, false
}

func (e *CursorExecutor) backdateConsumed(sessionKey, toolID string, age time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return
	}
	ref, ok := state.consumedIndex[toolID]
	if !ok {
		return
	}
	ref.consumedAt = time.Now().Add(-age)
	state.consumedIndex[toolID] = ref
}

func (e *CursorExecutor) conversationInvariantError() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.conversationInvariantErrorLocked()
}

func (e *CursorExecutor) expireStaleSessions() {
	var expired []cursorSessionReplaceEvent
	e.mu.Lock()
	e.expireStaleSessionsLocked(&expired)
	for k, cp := range e.checkpoints {
		if time.Since(cp.updatedAt) > cursorCheckpointTTL {
			delete(e.checkpoints, k)
		}
	}
	e.mu.Unlock()
	for _, event := range expired {
		reason := event.reason
		if reason == "" {
			reason = "ttl_expire"
		}
		logCursorSessionReplace(event.key, reason, event.old, nil)
	}
}
