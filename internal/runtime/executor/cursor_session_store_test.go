package executor

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"
)

func TestCursorSession_RandomizedOverlappingGenerations(t *testing.T) {
	rng := rand.New(rand.NewSource(20260819))
	for convN := 0; convN < 40; convN++ {
		sessionID := fmt.Sprintf("p0f-rand-%d", convN)
		e, conv, key, owner := p0cExecutor(sessionID)
		genCount := 2 + rng.Intn(4)
		type parked struct {
			genID string
			ids   []string
		}
		var gens []parked
		seen := map[string]struct{}{}
		for g := 0; g < genCount; g++ {
			nTools := 1 + rng.Intn(4)
			ids := make([]string, 0, nTools)
			for i := 0; i < nTools; i++ {
				id := normalizeToolCallID(fmt.Sprintf("p0f-%d-%d-%d-%d", convN, g, i, rng.Intn(1_000_000)))
				if _, ok := seen[id]; ok {
					continue
				}
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
			if len(ids) == 0 {
				continue
			}
			genID := fmt.Sprintf("gen-%d-%d", convN, g)
			p0cMustPark(t, e, conv, key, owner, genID, "req-"+genID, ids...)
			gens = append(gens, parked{genID: genID, ids: ids})
		}
		order := rng.Perm(len(gens))
		for _, idx := range order {
			item := gens[idx]
			if err := p0cSendResults(e, sessionID, "result-"+item.genID, item.ids...); err != nil {
				t.Fatalf("conv %d gen %s result failed: %v", convN, item.genID, err)
			}
		}
		if live := e.livePendingToolIDs(key); len(live) != 0 {
			t.Fatalf("conv %d leftover pending: %v", convN, live)
		}
	}
}

func TestCursorSession_DuplicateConsumedResult(t *testing.T) {
	idA := normalizeToolCallID("p0c-dup-A")
	sessionID := "p0c-duplicate"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
		t.Fatalf("first consume failed: %v", err)
	}
	p0cCommitResults(e, sessionID, idA)
	err := p0cSendResults(e, sessionID, "req-A-retry", idA)
	if err == nil || !strings.Contains(err.Error(), "TOOL_RESULT_ALREADY_CONSUMED") {
		t.Fatalf("duplicate result error = %v, want TOOL_RESULT_ALREADY_CONSUMED", err)
	}
}

func TestCursorSession_ReplayableResultsOnFinishedStreamKeepOriginalGeneration(t *testing.T) {
	idA := normalizeToolCallID("p0c-dead-A")
	sessionID := "p0c-dead-stream"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
		t.Fatalf("first consume failed: %v", err)
	}
	p0cInterruptResults(e, sessionID, cursorWatchdogErr(cursorReasonTransportIdle, time.Second), idA)
	e.mu.Lock()
	if state := e.conversations[key]; state != nil {
		if session := state.generations["gen-A"]; session != nil {
			session.finished = true
		}
	}
	e.mu.Unlock()
	session, err := e.resolveGenerationForToolResults(key, []string{idA})
	if err != nil {
		t.Fatalf("replayable retry error = %v, want original generation", err)
	}
	if session == nil || session.generationID != "gen-A" {
		t.Fatalf("replayable retry resolved %#v, want gen-A", session)
	}
	claim, err := e.claimToolResults(key, "req-A-retry", []string{idA})
	if err != nil {
		t.Fatalf("replayable claim error = %v", err)
	}
	if claim.generationID != "gen-A" || !claim.cold {
		t.Fatalf("claim = gen=%s cold=%v, want gen-A cold continuation", claim.generationID, claim.cold)
	}
}
