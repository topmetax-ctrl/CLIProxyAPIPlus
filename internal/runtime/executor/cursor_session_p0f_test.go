package executor

import (
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"
)

func TestCursorSession_MixedToolResultsRejectedWithoutMutation(t *testing.T) {
	idA := normalizeToolCallID("p0f-mixed-mut-A")
	idB := normalizeToolCallID("p0f-mixed-mut-B")
	sessionID := "p0f-mixed-mutation"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)

	if err := p0cSendResults(e, sessionID, "req-mixed", idA, idB); err == nil {
		t.Fatal("expected MIXED_TOOL_RESULT_GENERATIONS")
	}
	live := e.livePendingToolIDs(key)
	if !p0cContains(live, idA) || !p0cContains(live, idB) {
		t.Fatalf("mixed resolve mutated pending: %v", live)
	}
	if consumed := e.consumedToolIDs(key); len(consumed) != 0 {
		t.Fatalf("mixed resolve consumed tools: %v", consumed)
	}
	if err := e.conversationInvariantError(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorSession_KnownAndUnknownToolResultsAreAtomic(t *testing.T) {
	idA := normalizeToolCallID("p0f-atomic-A")
	idX := normalizeToolCallID("p0f-atomic-X")
	sessionID := "p0f-known-unknown"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)

	if err := p0cSendResults(e, sessionID, "req-known-unknown", idA, idX); err == nil {
		t.Fatal("expected TOOL_RESULT_NOT_FOUND")
	}
	if !p0cContains(e.livePendingToolIDs(key), idA) {
		t.Fatal("known+unknown consume must not commit tool-A")
	}
	if consumed := e.consumedToolIDs(key); p0cContains(consumed, idA) {
		t.Fatal("known+unknown left a consumed tombstone for tool-A")
	}
}

func TestCursorSession_ConcurrentPartialConsumeIsAtomic(t *testing.T) {
	id1 := normalizeToolCallID("p0f-conc-1")
	id2 := normalizeToolCallID("p0f-conc-2")
	sessionID := "p0f-concurrent-consume"
	e, conv, key, owner := p0cExecutor(sessionID)
	session := p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", id1, id2)

	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	consume := func(id string) {
		defer wg.Done()
		<-start
		got, err := e.resolveGenerationForToolResults(key, []string{id})
		if err != nil {
			errs <- err
			return
		}
		if got == nil {
			errs <- fmt.Errorf("resolve %s returned nil session", id)
			return
		}
		e.consumeToolResults(key, got, []string{id})
	}
	wg.Add(2)
	go consume(id1)
	go consume(id2)
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent consume: %v", err)
	}

	if live := e.livePendingToolIDs(key); len(live) != 0 {
		t.Fatalf("pending leftover after concurrent consume: %v", live)
	}
	consumed := e.consumedToolIDs(key)
	if !p0cContains(consumed, id1) || !p0cContains(consumed, id2) {
		t.Fatalf("consumed index missing tools: %v", consumed)
	}
	state, ok := e.generationStateOf(session.generationID)
	if !ok || state != generationConsumed {
		t.Fatalf("generation state = %s ok=%v, want CONSUMED", state, ok)
	}
	if err := e.conversationInvariantError(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorSession_HardExpireRemovesStalePendingWithoutCorruptingLive(t *testing.T) {
	idA := normalizeToolCallID("p0f-hard-A")
	idB := normalizeToolCallID("p0f-hard-B")
	sessionID := "p0f-hard-expire"
	e, conv, key, owner := p0cExecutor(sessionID)
	genA := p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	p0cMustPark(t, e, conv, key, owner, "gen-B", "req-B", idB)

	before := CursorSessionForcedExpireWithPendingTotal()
	e.backdateGeneration(genA.generationID, cursorSessionHardTTL+time.Minute)
	e.expireStaleSessions()

	if CursorSessionForcedExpireWithPendingTotal() <= before {
		t.Fatal("hard expire did not increment forced-expire-with-pending")
	}
	if p0cContains(e.livePendingToolIDs(key), idA) {
		t.Fatal("hard-expired gen-A is still pending")
	}
	if _, ok := e.generationStateOf(genA.generationID); ok {
		t.Fatal("hard-expired gen-A is still in the store")
	}
	if !p0cContains(e.livePendingToolIDs(key), idB) {
		t.Fatal("hard-expiring gen-A removed gen-B")
	}
	if err := p0cSendResults(e, sessionID, "req-B-result", idB); err != nil {
		t.Fatalf("live gen-B must still resolve: %v", err)
	}
}

func TestCursorSession_ConsumedIndexGC(t *testing.T) {
	idA := normalizeToolCallID("p0f-gc-A")
	sessionID := "p0f-consumed-gc"
	e, conv, key, owner := p0cExecutor(sessionID)
	p0cMustPark(t, e, conv, key, owner, "gen-A", "req-A", idA)
	if err := p0cSendResults(e, sessionID, "req-A-result", idA); err != nil {
		t.Fatalf("consume failed: %v", err)
	}
	if !p0cContains(e.consumedToolIDs(key), idA) {
		t.Fatal("expected consumed tombstone")
	}
	beforeGC := CursorSessionConsumedGCTotal()
	e.backdateConsumed(key, idA, cursorConsumedIndexTTL+time.Minute)
	e.expireStaleSessions()
	if CursorSessionConsumedGCTotal() <= beforeGC {
		t.Fatal("consumed index GC did not increment")
	}
	if p0cContains(e.consumedToolIDs(key), idA) {
		t.Fatal("consumed tombstone survived GC")
	}
	err := p0cSendResults(e, sessionID, "req-A-after-gc", idA)
	if err == nil || !isCursorLocalSessionError(err) {
		t.Fatalf("after tombstone GC, duplicate should be a local miss, got %v", err)
	}
}

func TestCursorSession_RandomizedStateMachine(t *testing.T) {
	rng := rand.New(rand.NewSource(20260819))
	e := NewCursorExecutor(nil)
	const conversations = 24
	const ops = 8000

	type liveGen struct {
		id   string
		key  string
		conv string
		ids  []string
	}
	owners := make(map[string]*cursorStateOwner)
	var gens []liveGen
	next := 0
	newID := func() string {
		next++
		return normalizeToolCallID(fmt.Sprintf("p0f-sm-%d-%d", next, rng.Intn(1_000_000)))
	}
	ensureOwner := func(conv string) *cursorStateOwner {
		if owner := owners[conv]; owner != nil {
			return owner
		}
		owner := e.beginConversationStream(conv)
		owners[conv] = owner
		return owner
	}

	for i := 0; i < ops; i++ {
		switch rng.Intn(8) {
		case 0, 1, 2: // park
			convN := rng.Intn(conversations)
			sessionID := fmt.Sprintf("p0f-sm-%d", convN)
			conv := deriveConversationId("", sessionID, "")
			key := "cursor-test:" + conv
			nTools := 1 + rng.Intn(4)
			ids := make([]string, 0, nTools)
			for j := 0; j < nTools; j++ {
				ids = append(ids, newID())
			}
			genID := fmt.Sprintf("gen-%d-%d", convN, next)
			session := &cursorSession{
				pending:         p0fPending(ids),
				cancel:          func() {},
				createdAt:       time.Now(),
				updatedAt:       time.Now(),
				authID:          "cursor-test",
				generationID:    genID,
				sourceRequestID: "req-" + genID,
			}
			if e.parkGeneration(conv, key, ensureOwner(conv), session) {
				gens = append(gens, liveGen{id: genID, key: key, conv: conv, ids: ids})
			}
		case 3, 4: // consume some pending IDs from one generation
			if len(gens) == 0 {
				continue
			}
			idx := rng.Intn(len(gens))
			item := gens[idx]
			if len(item.ids) == 0 {
				continue
			}
			n := 1 + rng.Intn(len(item.ids))
			incoming := append([]string(nil), item.ids[:n]...)
			session, err := e.resolveGenerationForToolResults(item.key, incoming)
			if err != nil {
				t.Fatalf("op %d resolve: %v", i, err)
			}
			if session == nil {
				t.Fatalf("op %d resolve missed live generation %s", i, item.id)
			}
			e.consumeToolResults(item.key, session, incoming)
			item.ids = item.ids[n:]
			if len(item.ids) == 0 {
				gens = append(gens[:idx], gens[idx+1:]...)
			} else {
				gens[idx] = item
			}
		case 5: // duplicate a consumed ID if any remain in an index
			if len(gens) == 0 {
				continue
			}
			item := gens[rng.Intn(len(gens))]
			consumed := e.consumedToolIDs(item.key)
			if len(consumed) == 0 {
				continue
			}
			_, err := e.resolveGenerationForToolResults(item.key, []string{consumed[0]})
			if err == nil || !isCursorLocalSessionError(err) {
				t.Fatalf("op %d duplicate got %v", i, err)
			}
		case 6: // unknown / mixed must not mutate
			if len(gens) < 2 {
				continue
			}
			a := gens[rng.Intn(len(gens))]
			b := gens[rng.Intn(len(gens))]
			if a.key != b.key || a.id == b.id || len(a.ids) == 0 || len(b.ids) == 0 {
				_, err := e.resolveGenerationForToolResults(a.key, []string{newID()})
				if err == nil || !isCursorLocalSessionError(err) {
					t.Fatalf("op %d unknown got %v", i, err)
				}
				break
			}
			before := append([]string(nil), e.livePendingToolIDs(a.key)...)
			_, err := e.resolveGenerationForToolResults(a.key, []string{a.ids[0], b.ids[0]})
			if err == nil || !isCursorLocalSessionError(err) {
				t.Fatalf("op %d mixed got %v", i, err)
			}
			after := e.livePendingToolIDs(a.key)
			if len(before) != len(after) {
				t.Fatalf("op %d mixed mutated pending %v -> %v", i, before, after)
			}
		default: // soft/hard expire a generation
			if len(gens) == 0 {
				continue
			}
			idx := rng.Intn(len(gens))
			item := gens[idx]
			if rng.Intn(4) == 0 {
				e.backdateGeneration(item.id, cursorSessionHardTTL+time.Minute)
			} else {
				e.backdateGeneration(item.id, cursorSessionTTL+time.Minute)
			}
			e.expireStaleSessions()
			if _, ok := e.generationStateOf(item.id); !ok {
				gens = append(gens[:idx], gens[idx+1:]...)
			}
		}
		if err := e.conversationInvariantError(); err != nil {
			t.Fatalf("op %d invariant: %v", i, err)
		}
	}
}

func p0fPending(ids []string) []pendingMcpExec {
	out := make([]pendingMcpExec, 0, len(ids))
	for _, id := range ids {
		out = append(out, pendingMcpExec{ToolCallId: id, ToolName: "read", Args: `{}`})
	}
	return out
}
