package executor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	cursorproto "github.com/router-for-me/CLIProxyAPI/v7/internal/auth/cursor/proto"
	log "github.com/sirupsen/logrus"
)

type resultState int

const (
	resultInFlight resultState = iota + 1
	resultCommitted
	resultReplayable
	resultFinalRejected
)

func (s resultState) String() string {
	switch s {
	case resultInFlight:
		return "IN_FLIGHT"
	case resultCommitted:
		return "COMMITTED"
	case resultReplayable:
		return "REPLAYABLE"
	case resultFinalRejected:
		return "FINAL_REJECTED"
	default:
		return fmt.Sprintf("resultState(%d)", int(s))
	}
}

type resultAttempt struct {
	id                          string
	requestID                   string
	startedAt                   time.Time
	upstreamStarted             bool
	upstreamTerminal            bool
	downstreamTerminalDelivered bool
}

type toolResultRecord struct {
	generationID     string
	state            resultState
	attempt          resultAttempt
	claimedAt        time.Time
	claimDeadline    time.Time
	lastTransitionAt time.Time
	coldReplay       bool
}

type toolResultClaim struct {
	generationID    string
	session         *cursorSession
	ids             []string
	cold            bool
	attemptID       string
	previousPending []pendingMcpExec
}

// Lease must outlive a normal request, including transport idle and max duration.
var cursorToolResultClaimLease = cursorSessionHardTTL + 2*time.Minute

func (e *CursorExecutor) recoverStaleClaimsLocked(state *conversationState, now time.Time) {
	if state == nil {
		return
	}
	for _, rec := range state.resultIndex {
		if rec == nil || rec.state != resultInFlight {
			continue
		}
		if rec.claimDeadline.IsZero() || !now.After(rec.claimDeadline) {
			continue
		}
		e.transitionResultLocked(rec, resultReplayable, "stale_claim_lease")
		rec.coldReplay = true
		cursorToolResultStaleClaimRecoveredTotal.Add(1)
	}
}

func (e *CursorExecutor) transitionResultLocked(rec *toolResultRecord, to resultState, reason string) {
	if rec == nil {
		return
	}
	from := rec.state
	rec.state = to
	rec.lastTransitionAt = time.Now()
	if to == resultReplayable {
		rec.coldReplay = true
	}
	log.WithFields(log.Fields{
		"event":           "cursor_tool_result_state_transition",
		"generation_hash": cursorToolIDHash(rec.generationID),
		"attempt_id":      rec.attempt.id,
		"request_id":      rec.attempt.requestID,
		"from":            from.String(),
		"to":              to.String(),
		"reason":          reason,
	}).Info("cursor tool result state transition")
}

func classifyFrontierLocked(state *conversationState, incoming []string) (pending, replayable, inFlight, committed, rejected, unknown []string, claimGens map[string]struct{}) {
	claimGens = make(map[string]struct{})
	if state == nil {
		unknown = append([]string(nil), incoming...)
		return
	}
	for _, id := range uniqueSortedStrings(incoming) {
		if genID, ok := state.pendingIndex[id]; ok {
			pending = append(pending, id)
			claimGens[genID] = struct{}{}
			continue
		}
		rec := state.resultIndex[id]
		if rec == nil {
			unknown = append(unknown, id)
			continue
		}
		switch rec.state {
		case resultReplayable:
			replayable = append(replayable, id)
			claimGens[rec.generationID] = struct{}{}
		case resultInFlight:
			inFlight = append(inFlight, id)
		case resultCommitted:
			committed = append(committed, id)
		case resultFinalRejected:
			rejected = append(rejected, id)
		default:
			unknown = append(unknown, id)
		}
	}
	return
}

func validateFrontier(pending, replayable, inFlight, committed, rejected, unknown []string, claimGens map[string]struct{}) error {
	claimable := len(pending) + len(replayable)
	if len(unknown) > 0 {
		cursorSessionGenerationLookupMissTotal.Add(1)
		return cursorLocalError(localToolResultNotFound, http.StatusBadRequest, errToolResultNotFound)
	}
	if len(inFlight) > 0 {
		cursorToolResultInFlightConflictTotal.Add(1)
		return cursorLocalError(localInFlightResult, http.StatusConflict, errToolResultInFlight)
	}
	if len(claimGens) > 1 {
		cursorToolResultMixedGenerationTotal.Add(1)
		return cursorLocalError(localMixedGeneration, http.StatusConflict, errMixedToolResultGenerations)
	}
	if claimable == 0 && len(committed) > 0 {
		cursorGenerationDuplicateResultTotal.Add(1)
		return cursorLocalError(localDuplicateResult, http.StatusConflict, errToolResultAlreadyConsumed)
	}
	if claimable == 0 && len(rejected) > 0 {
		cursorToolResultFinalRejectTotal.Add(1)
		return cursorLocalError(localFinalRejectedResult, http.StatusBadRequest, errToolResultFinalRejected)
	}
	_ = pending
	_ = replayable
	return nil
}

func (e *CursorExecutor) inspectToolResultsLocked(sessionKey string, incoming []string) (*cursorSession, error) {
	state := e.conversations[sessionKey]
	if state == nil {
		cursorSessionGenerationLookupMissTotal.Add(1)
		return nil, nil
	}
	e.recoverStaleClaimsLocked(state, time.Now())
	pending, replayable, inFlight, committed, rejected, unknown, claimGens := classifyFrontierLocked(state, incoming)
	if err := validateFrontier(pending, replayable, inFlight, committed, rejected, unknown, claimGens); err != nil {
		return nil, err
	}
	if len(claimGens) == 0 {
		cursorSessionGenerationLookupMissTotal.Add(1)
		return nil, nil
	}
	var genID string
	for id := range claimGens {
		genID = id
	}
	cursorGenerationResolveTotal.Add(1)
	return state.generations[genID], nil
}

func (e *CursorExecutor) claimToolResults(sessionKey, requestID string, incoming []string) (*toolResultClaim, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.claimToolResultsLocked(sessionKey, requestID, incoming)
}

func (e *CursorExecutor) claimToolResultsLocked(sessionKey, requestID string, incoming []string) (*toolResultClaim, error) {
	state := e.conversations[sessionKey]
	if state == nil {
		cursorSessionGenerationLookupMissTotal.Add(1)
		return nil, cursorLocalError(localToolResultNotFound, http.StatusBadRequest, errToolResultNotFound)
	}
	e.recoverStaleClaimsLocked(state, time.Now())
	pending, replayable, inFlight, committed, rejected, unknown, claimGens := classifyFrontierLocked(state, incoming)
	if err := validateFrontier(pending, replayable, inFlight, committed, rejected, unknown, claimGens); err != nil {
		return nil, err
	}
	if len(claimGens) == 0 {
		cursorSessionGenerationLookupMissTotal.Add(1)
		return nil, cursorLocalError(localToolResultNotFound, http.StatusBadRequest, errToolResultNotFound)
	}
	var genID string
	for id := range claimGens {
		genID = id
	}
	session := state.generations[genID]
	now := time.Now()
	attemptID := uuid.New().String()
	ids := append(append([]string(nil), pending...), replayable...)
	cold := session == nil || session.finished || session.stream == nil || session.toolResultCh == nil
	var previousPending []pendingMcpExec
	if session != nil {
		previousPending = append([]pendingMcpExec(nil), session.pending...)
	}
	replayClaim := len(replayable) > 0
	for _, id := range ids {
		from := "PENDING"
		if prev := state.resultIndex[id]; prev != nil {
			from = prev.state.String()
			if prev.coldReplay || prev.state == resultReplayable {
				cold = true
				replayClaim = true
			}
		}
		rec := &toolResultRecord{
			generationID: genID,
			state:        resultInFlight,
			attempt: resultAttempt{
				id:        attemptID,
				requestID: requestID,
				startedAt: now,
			},
			claimedAt:        now,
			claimDeadline:    now.Add(cursorToolResultClaimLease),
			lastTransitionAt: now,
			coldReplay:       cold,
		}
		delete(state.pendingIndex, id)
		state.resultIndex[id] = rec
		log.WithFields(log.Fields{
			"event":           "cursor_tool_result_state_transition",
			"generation_hash": cursorToolIDHash(genID),
			"attempt_id":      attemptID,
			"request_id":      requestID,
			"from":            from,
			"to":              resultInFlight.String(),
			"reason":          "claim",
		}).Info("cursor tool result state transition")
	}
	if session != nil {
		claimed := make(map[string]struct{}, len(ids))
		for _, id := range ids {
			claimed[id] = struct{}{}
		}
		remaining := session.pending[:0]
		for _, item := range session.pending {
			if _, ok := claimed[item.ToolCallId]; ok {
				continue
			}
			remaining = append(remaining, item)
		}
		session.pending = remaining
		session.updatedAt = now
		if len(session.pending) == 0 {
			session.state = generationConsumed
			session.consumedAt = now
		} else {
			session.state = generationPartiallyConsumed
		}
	}
	cursorToolResultClaimTotal.Add(1)
	if replayClaim {
		cursorToolResultReplayTotal.Add(1)
	}
	e.refreshConsumedIndexGaugeLocked()
	return &toolResultClaim{
		generationID:    genID,
		session:         session,
		ids:             ids,
		cold:            cold,
		attemptID:       attemptID,
		previousPending: previousPending,
	}, nil
}

func (e *CursorExecutor) unclaimToolResults(sessionKey string, claim *toolResultClaim, previous []pendingMcpExec) {
	if claim == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return
	}
	for _, id := range claim.ids {
		rec := state.resultIndex[id]
		if rec == nil {
			continue
		}
		if rec.coldReplay || rec.attempt.id != claim.attemptID {
			e.transitionResultLocked(rec, resultReplayable, "unclaim")
			continue
		}
		delete(state.resultIndex, id)
		state.pendingIndex[id] = claim.generationID
	}
	if claim.session != nil && previous != nil {
		claim.session.pending = append([]pendingMcpExec(nil), previous...)
		claim.session.state = generationParked
		claim.session.consumedAt = time.Time{}
	}
	e.refreshConsumedIndexGaugeLocked()
}

func (e *CursorExecutor) markToolResultsUpstreamStarted(sessionKey string, ids []string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return
	}
	now := time.Now()
	for _, id := range ids {
		rec := state.resultIndex[id]
		if rec == nil || rec.state != resultInFlight {
			continue
		}
		rec.attempt.upstreamStarted = true
		rec.lastTransitionAt = now
	}
}

func (e *CursorExecutor) commitToolResults(sessionKey string, ids []string, downstreamDelivered bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return
	}
	for _, id := range ids {
		rec := state.resultIndex[id]
		if rec == nil || rec.state != resultInFlight {
			continue
		}
		rec.attempt.upstreamTerminal = true
		rec.attempt.downstreamTerminalDelivered = downstreamDelivered
		if !downstreamDelivered {
			e.transitionResultLocked(rec, resultReplayable, "downstream_not_delivered")
			cursorToolResultReplayTotal.Add(1)
			continue
		}
		e.transitionResultLocked(rec, resultCommitted, "terminal_success")
		cursorToolResultCommitTotal.Add(1)
	}
	e.refreshConsumedIndexGaugeLocked()
}

func (e *CursorExecutor) interruptToolResults(sessionKey string, ids []string, streamErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return
	}
	reason := "interrupted"
	to := resultReplayable
	if streamErr != nil && !isCursorReplayableInterrupt(streamErr) {
		to = resultFinalRejected
		reason = "final_upstream_rejection"
	}
	for _, id := range ids {
		rec := state.resultIndex[id]
		if rec == nil || rec.state != resultInFlight {
			continue
		}
		e.transitionResultLocked(rec, to, reason)
		if to == resultReplayable {
			cursorToolResultReplayTotal.Add(1)
		} else {
			cursorToolResultFinalRejectTotal.Add(1)
		}
	}
	e.refreshConsumedIndexGaugeLocked()
}

func isCursorReplayableInterrupt(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if isCursorLocalSessionError(err) {
		return false
	}
	var ce *cursorproto.ConnectError
	if errors.As(err, &ce) {
		switch ce.Code {
		case "invalid_argument", "failed_precondition", "out_of_range",
			"already_exists", "unauthenticated", "permission_denied",
			"not_found", "unimplemented":
			return false
		default:
			return true
		}
	}
	classified := classifyCursorError(err)
	se, ok := classified.(interface{ StatusCode() int })
	if !ok {
		return true
	}
	switch se.StatusCode() {
	case 401, 403, 404:
		return false
	case 400:
		msg := strings.ToLower(err.Error())
		return strings.Contains(msg, "rst_stream") || strings.Contains(msg, "goaway")
	case 429, 500, 502, 503, 504:
		return true
	default:
		return true
	}
}

func (e *CursorExecutor) inflightResultIDs(sessionKey, generationID string) []string {
	if generationID == "" {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return nil
	}
	ids := make([]string, 0)
	for id, rec := range state.resultIndex {
		if rec != nil && rec.state == resultInFlight && rec.generationID == generationID {
			ids = append(ids, id)
		}
	}
	return ids
}

func (e *CursorExecutor) finishClaimedToolResults(sessionKey string, ids []string, streamErr error, downstreamDelivered bool) {
	if len(ids) == 0 {
		return
	}
	if streamErr != nil {
		e.interruptToolResults(sessionKey, ids, streamErr)
		return
	}
	e.commitToolResults(sessionKey, ids, downstreamDelivered)
}

func (e *CursorExecutor) resultStateOf(sessionKey, toolID string) (resultState, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return 0, false
	}
	rec := state.resultIndex[toolID]
	if rec == nil {
		return 0, false
	}
	return rec.state, true
}

func (e *CursorExecutor) backdateClaim(sessionKey, toolID string, age time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	state := e.conversations[sessionKey]
	if state == nil {
		return
	}
	rec := state.resultIndex[toolID]
	if rec == nil {
		return
	}
	rec.claimedAt = time.Now().Add(-age)
	rec.claimDeadline = rec.claimedAt.Add(cursorToolResultClaimLease)
}
