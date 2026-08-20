package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

func TestToolResult_TimeoutThenRetrySameResult_ReplaysOriginalGeneration(t *testing.T) {
	idA := normalizeToolCallID("p0k2-timeout-A")
	sessionID := "p0k2-timeout-retry"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)

	claim1, err := e.claimToolResults(key, "req-R1", []string{idA})
	if err != nil {
		t.Fatalf("R1 claim: %v", err)
	}
	if claim1.generationID != "gen-A" {
		t.Fatalf("R1 generation = %s, want gen-A", claim1.generationID)
	}
	e.interruptToolResults(key, []string{idA}, cursorWatchdogErr(cursorReasonTransportIdle, 4*time.Minute))
	if st, ok := e.resultStateOf(key, idA); !ok || st != resultReplayable {
		t.Fatalf("after 504 state = %s ok=%v, want REPLAYABLE", st, ok)
	}

	claim2, err := e.claimToolResults(key, "req-R2", []string{idA})
	if err != nil {
		t.Fatalf("R2 claim: %v", err)
	}
	if claim2.generationID != "gen-A" {
		t.Fatalf("R2 resolved %s, want original gen-A (no newest-generation fallback)", claim2.generationID)
	}
	if !claim2.cold {
		t.Fatal("interrupted attempt must cold-continue, not resume a dead H2 stream")
	}
}

func TestToolResult_ReplayAWhileBPending(t *testing.T) {
	idA := normalizeToolCallID("p0k2-replayA-A")
	idB := normalizeToolCallID("p0k2-replayA-B")
	sessionID := "p0k2-replay-a-while-b"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if _, err := e.claimToolResults(key, "req-R1", []string{idA}); err != nil {
		t.Fatalf("claim A: %v", err)
	}
	e.interruptToolResults(key, []string{idA}, cursorWatchdogErr(cursorReasonFirstByteTimeout, time.Second))
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)

	claim, err := e.claimToolResults(key, "req-R2", []string{idA})
	if err != nil {
		t.Fatalf("retry A: %v", err)
	}
	if claim.generationID != "gen-A" {
		t.Fatalf("retry A resolved %s, want gen-A", claim.generationID)
	}
	if !p0cContains(e.livePendingToolIDs(key), idB) {
		t.Fatal("gen-B pending was lost while replaying A")
	}
}

func TestToolResult_RetryWhileOriginalInFlight(t *testing.T) {
	idA := normalizeToolCallID("p0k2-inflight-A")
	sessionID := "p0k2-inflight"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if err := p0cSendResults(e, sessionID, "req-R1", idA); err != nil {
		t.Fatalf("R1: %v", err)
	}
	err := p0cSendResults(e, sessionID, "req-R2", idA)
	if !errors.Is(err, errToolResultInFlight) {
		t.Fatalf("R2 error = %v, want TOOL_RESULT_IN_FLIGHT", err)
	}
	if st, ok := e.resultStateOf(key, idA); !ok || st != resultInFlight {
		t.Fatalf("state = %s ok=%v, want IN_FLIGHT", st, ok)
	}
}

func TestToolResult_StaleInflightLease(t *testing.T) {
	idA := normalizeToolCallID("p0k2-lease-A")
	sessionID := "p0k2-stale-lease"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if _, err := e.claimToolResults(key, "req-R1", []string{idA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	before := CursorToolResultStaleClaimRecoveredTotal()
	e.backdateClaim(key, idA, cursorToolResultClaimLease+time.Second)
	claim, err := e.claimToolResults(key, "req-R2", []string{idA})
	if err != nil {
		t.Fatalf("stale lease claim: %v", err)
	}
	if claim.generationID != "gen-A" {
		t.Fatalf("recovered claim gen = %s, want gen-A", claim.generationID)
	}
	if CursorToolResultStaleClaimRecoveredTotal() <= before {
		t.Fatal("stale claim recovery did not increment")
	}
}

func TestToolResult_TerminalSuccessThenActiveDuplicate(t *testing.T) {
	idA := normalizeToolCallID("p0k2-dup-A")
	sessionID := "p0k2-terminal-dup"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if err := p0cSendResults(e, sessionID, "req-R1", idA); err != nil {
		t.Fatalf("first: %v", err)
	}
	e.commitToolResults(key, []string{idA}, true)
	err := p0cSendResults(e, sessionID, "req-R2", idA)
	if !errors.Is(err, errToolResultAlreadyConsumed) {
		t.Fatalf("duplicate error = %v, want TOOL_RESULT_ALREADY_CONSUMED", err)
	}
}

func TestToolResult_HistoricalCommittedInTranscript(t *testing.T) {
	idA := normalizeToolCallID("p0k2-hist-A")
	idB := normalizeToolCallID("p0k2-hist-B")
	sessionID := "p0k2-historical"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if _, err := e.claimToolResults(key, "req-A", []string{idA}); err != nil {
		t.Fatalf("claim A: %v", err)
	}
	e.commitToolResults(key, []string{idA}, true)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)

	payload := p0cClaudeHistoryThenCurrentPayload(sessionID, idA, idB)
	_, err := e.ExecuteStream(
		logging.WithRequestID(context.Background(), "req-B"),
		cursorTestAuth(),
		cliproxyexecutor.Request{Model: "cursor-test-model", Payload: payload},
		p0bClaudeOpts(payload),
	)
	if err != nil {
		t.Fatalf("historical COMMITTED A must be ignored: %v", err)
	}
	if p0cContains(e.livePendingToolIDs(key), idB) {
		t.Fatal("current-turn B should be claimed")
	}
	if st, ok := e.resultStateOf(key, idA); !ok || st != resultCommitted {
		t.Fatalf("historical A state = %s ok=%v, want COMMITTED", st, ok)
	}
}

func TestToolResult_PendingAndReplayableSameGeneration(t *testing.T) {
	id1 := normalizeToolCallID("p0k2-partial-1")
	id2 := normalizeToolCallID("p0k2-partial-2")
	sessionID := "p0k2-pending-replayable"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", id1, id2)
	if _, err := e.claimToolResults(key, "req-partial", []string{id1}); err != nil {
		t.Fatalf("claim id1: %v", err)
	}
	e.interruptToolResults(key, []string{id1}, cursorWatchdogErr(cursorReasonTransportIdle, time.Second))

	claim, err := e.claimToolResults(key, "req-rest", []string{id1, id2})
	if err != nil {
		t.Fatalf("atomic pending+replayable claim: %v", err)
	}
	if claim.generationID != "gen-A" {
		t.Fatalf("generation = %s, want gen-A", claim.generationID)
	}
	if len(claim.ids) != 2 {
		t.Fatalf("claimed %d ids, want 2", len(claim.ids))
	}
}

func TestToolResult_KnownAndUnknownZeroMutation(t *testing.T) {
	idA := normalizeToolCallID("p0k2-known-A")
	idX := normalizeToolCallID("p0k2-unknown-X")
	sessionID := "p0k2-known-unknown"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	before := append([]string(nil), e.livePendingToolIDs(key)...)
	_, err := e.claimToolResults(key, "req-bad", []string{idA, idX})
	if !errors.Is(err, errToolResultNotFound) {
		t.Fatalf("error = %v, want NOT_FOUND", err)
	}
	after := e.livePendingToolIDs(key)
	if len(before) != len(after) || !p0cContains(after, idA) {
		t.Fatalf("pending mutated %v -> %v", before, after)
	}
}

func TestToolResult_MixedGenerationsZeroMutation(t *testing.T) {
	idA := normalizeToolCallID("p0k2-mix-A")
	idB := normalizeToolCallID("p0k2-mix-B")
	sessionID := "p0k2-mixed-gens"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)
	_, err := e.claimToolResults(key, "req-mixed", []string{idA, idB})
	if !errors.Is(err, errMixedToolResultGenerations) {
		t.Fatalf("error = %v, want MIXED", err)
	}
	live := e.livePendingToolIDs(key)
	if !p0cContains(live, idA) || !p0cContains(live, idB) {
		t.Fatalf("mixed claim mutated pending: %v", live)
	}
}

func TestToolResult_DownstreamDisconnect(t *testing.T) {
	idA := normalizeToolCallID("p0k2-down-A")
	sessionID := "p0k2-downstream"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if _, err := e.claimToolResults(key, "req-R1", []string{idA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	e.commitToolResults(key, []string{idA}, false)
	if st, ok := e.resultStateOf(key, idA); !ok || st != resultReplayable {
		t.Fatalf("state = %s ok=%v, want REPLAYABLE", st, ok)
	}
}

func TestToolResult_UpstreamReset(t *testing.T) {
	idA := normalizeToolCallID("p0k2-reset-A")
	sessionID := "p0k2-upstream-reset"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if _, err := e.claimToolResults(key, "req-R1", []string{idA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	e.interruptToolResults(key, []string{idA}, fmt.Errorf("http2: stream closed: RST_STREAM"))
	if st, ok := e.resultStateOf(key, idA); !ok || st != resultReplayable {
		t.Fatalf("state = %s ok=%v, want REPLAYABLE", st, ok)
	}
}

func TestToolResult_FinalProviderRejection(t *testing.T) {
	idA := normalizeToolCallID("p0k2-reject-A")
	sessionID := "p0k2-final-reject"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if _, err := e.claimToolResults(key, "req-R1", []string{idA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	e.interruptToolResults(key, []string{idA}, cursorStatusErr{code: 400, msg: "cursor: invalid_argument"})
	if st, ok := e.resultStateOf(key, idA); !ok || st != resultFinalRejected {
		t.Fatalf("state = %s ok=%v, want FINAL_REJECTED", st, ok)
	}
	_, err := e.claimToolResults(key, "req-R2", []string{idA})
	if !errors.Is(err, errToolResultFinalRejected) {
		t.Fatalf("retry error = %v, want FINAL_REJECTED", err)
	}
}

func TestToolResult_ConcurrentDuplicateRetries(t *testing.T) {
	idA := normalizeToolCallID("p0k2-conc-A")
	sessionID := "p0k2-concurrent"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	claim := func() {
		defer wg.Done()
		<-start
		_, err := e.claimToolResults(key, "req-conc", []string{idA})
		errs <- err
	}
	wg.Add(2)
	go claim()
	go claim()
	close(start)
	wg.Wait()
	close(errs)

	var ok, conflict int
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, errToolResultInFlight):
			conflict++
		default:
			t.Fatalf("concurrent claim error = %v", err)
		}
	}
	if ok != 1 || conflict != 1 {
		t.Fatalf("claimants ok=%d conflict=%d, want 1/1", ok, conflict)
	}
}

func TestToolResult_PartialStreamThenResetIsReplayable(t *testing.T) {
	idA := normalizeToolCallID("p0k2-partial-A")
	sessionID := "p0k2-partial-stream"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if _, err := e.claimToolResults(key, "req-R1", []string{idA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	e.markToolResultsUpstreamStarted(key, []string{idA})
	e.interruptToolResults(key, []string{idA}, cursorWatchdogErr(cursorReasonTransportIdle, 4*time.Minute))
	if st, ok := e.resultStateOf(key, idA); !ok || st != resultReplayable {
		t.Fatalf("partial thinking/text/heartbeat then reset: state = %s ok=%v, want REPLAYABLE", st, ok)
	}
}

func TestToolResult_ColdContinuationDoesNotResumeFinishedStream(t *testing.T) {
	idA := normalizeToolCallID("p0k2-cold-A")
	sessionID := "p0k2-cold-h2"
	opened := 0
	e := newCursorExecutorHarness(func(_ context.Context, _ cursorStream, _ map[string][]byte, _ anyMCPTools, onText func(string, bool), _ func([]pendingMcpExec), _ <-chan []toolResultInfo, _ *cursorTokenUsage, _ func([]byte)) error {
		onText("cold replay", false)
		return nil
	})
	e.openStream = func(string) (cursorStream, error) {
		opened++
		return newFakeCursorStream(), nil
	}
	conv := deriveConversationId("", sessionID, "")
	key := "cursor-test:" + conv
	owner := e.beginConversationStream(conv)
	session := p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if _, err := e.claimToolResults(key, "req-R1", []string{idA}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	e.interruptToolResults(key, []string{idA}, cursorWatchdogErr(cursorReasonMaxDuration, time.Minute))
	session.finished = true

	payload := p0cClaudeResultsPayload(sessionID, []string{idA})
	result, err := e.ExecuteStream(
		logging.WithRequestID(context.Background(), "req-R2"),
		cursorTestAuth(),
		cliproxyexecutor.Request{Model: "cursor-test-model", Payload: payload},
		p0bClaudeOpts(payload),
	)
	if err != nil {
		t.Fatalf("cold continuation ExecuteStream: %v", err)
	}
	_ = collectCursorStream(t, result)
	if opened == 0 {
		t.Fatal("finished H2 retry did not open a new stream")
	}
}
