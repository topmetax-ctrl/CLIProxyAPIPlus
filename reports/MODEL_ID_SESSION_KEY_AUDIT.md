# Model-ID patch vs Cursor session key — code audit

Status: **no production change**.  
**GO FOR IMPLEMENTATION: NO** (matcher and model-ID revert both still no)

Range:

```
GOOD = 08d66ec4  feat(management): surface Cursor provider quota on the control panel
BAD  = 5ecb91a3  fix(handlers): decode cloaked Anthropic model IDs on every request path
HEAD = 23ed67cb  docs(cursor): native tool rejection comment only
```

The 18/08 date in chat is off by one: the model-ID commit landed **2026-08-19 14:24 +0700**, about two hours before the 16:43 incident. Time correlation is real. Session-key causation is not.

---

## Verdicts (requested wording)

| Claim | Verdict |
|---|---|
| DIRECT tool-ID mutation regression from model-ID patch | **DISPROVEN** |
| Park/restore uses two different model representations | **DISPROVEN** |
| Session key includes model, so alias normalize merges buckets | **DISPROVEN** |
| Model-ID patch changed `cursor_executor` park/restore | **DISPROVEN** (`cursor_executor.go` untouched except a later comment) |
| INDIRECT routing/session-key regression | **DISPROVEN for session-key cardinality**; routing decode still happens, but the Cursor park key does not contain model |
| Latent session race (one slot / replace=true) | **CONFIRMED in code, pre-existing** |
| That race matches 16:43 symptoms | **HIGH, still needs PARK/RESTORE hashes on a live miss** |
| Model-ID patch as the trigger that made 16:43 appear | **LOW for this incident** (inbound model was already `cursor-grok-4.6-xhigh-fast`) |
| Model-ID patch as a general traffic-shape trigger | **OPEN, low** (see GetProviderName) |

Do not revert `5ecb91a3`. Do not fuzzy-match tool IDs.

---

## 1. Diff GOOD→BAD (Go, non-test)

`5ecb91a3` touches handlers, Claude model ID helpers, `GetProviderName`, listing cloak config. It does **not** touch:

- `internal/runtime/executor/cursor_executor.go`
- `publishConversationSession` / `resumeWithToolResults` / `deriveConversationId`
- pending matcher

HEAD after that only adds a comment in `processH2SessionFrames`.

---

## 2. Where model participates

| Key | Includes model? | Notes |
|---|---|---|
| Cursor `sessionKey` | **No** | `authID + ":" + conversationId` |
| `conversationId` | **No** | `sha256("cursor-conv:" + apiKey + ":" + sessionId)` or system-prompt fallback |
| `checkpoints` | **No** | keyed by `conversationId` only |
| `stateOwners` | **No** | keyed by `conversationId` |
| Pending match | **No** | exact `ToolCallId` string |
| Route / provider | **Yes** | `ResolveClaudeModelIDPrefix` then `GetProviderName` |
| Cache | n/a for Cursor H2 park | |
| Dead `deriveSessionKey(clientKey, model, messages)` | would include model | **never called** |

So this structure is **not** true in CLIProxy Cursor code:

```
session key = (session, model)
```

The live map is:

```
e.sessions[authID + ":" + conversationId] = *cursorSession  // one slot
```

`publishConversationSession(..., replace=true)` on park **overwrites** that slot. A later resume loads whatever generation currently sits there. If that generation’s pending IDs are disjoint from the inbound results → local 500. That is exactly the 16:43 shape, and it does not need model aliases to happen.

Same Claude Code `session_id` (16:43: `ef43570c-…`) + same cliproxy API key ⇒ **one** park bucket for main agent, retry, `continue`, and any subagent that reuses `metadata.user_id.session_id`, **even if they requested different Cursor models**. That was true before `5ecb91a3`.

---

## 3. Call order before vs after `rewriteClaudeDDModelInBody`

### `/v1/messages` before `5ecb91a3`

```
ClaudeMessages
  rewriteClaudeDDModelInBody(rawJSON)     // decode cloaked listing ID in JSON
  handleStreamingResponse
    ExecuteStream(modelName, rawJSON)
      deriveConversationId(apiKey, sessionId, systemPrompt)  // no model
      sessionKey = authID + ":" + conversationId
      restore or park
```

### `/v1/messages` after `5ecb91a3`

```
ClaudeMessages
  handleStreamingResponse                 // body may still be cloaked here
    executeStreamWithAuthManagerFormats
      canonicalizeRequestedModel          // decode name + RewriteModelField(body)
      applyModelRouter / GetProviderName
      ExecuteStream(canonical model, rewritten body)
        deriveConversationId(...)         // still no model
        sessionKey = authID + ":" + conversationId
        restore or park
```

Decode moved from the Claude handler into the shared execute/stream path (“every request path”). Park and restore both run **after** canonicalize, on the **same** derivation. There is no “park with decoded model, restore with cloaked model” split inside the executor: the executor never puts model into the key.

`originalRequestedModel` is kept as the inbound string for metadata/echo only (`handlers_execution.go` / `handlers_stream.go`).

### 16:43 specifically

Kiro-Go already rewrote `claude-opus-5-thinking` → `cursor-grok-4.6-xhigh-fast` before CLIProxy. `ResolveClaudeModelIDPrefix("cursor-grok-4.6-xhigh-fast")` is a no-op. The model-ID patch did not run a meaningful rewrite on that body.

---

## 4. What the patch *did* change (routing, not park key)

1. **Listing cloak is opt-in** (`cloak-model-list`, default off). Previously listing cloaked unless disabled.
2. **New cloak format** `claude-fable-5-dd-raw.<id>` instead of character-reversed payload.
3. **Decode on OpenAI/count/stream paths**, not only `ClaudeMessages`.
4. **`GetProviderName` no longer falls unknown IDs through to Cursor.**

(4) is the opposite of “more aliases collapse onto Cursor”. Unknown/garbage IDs now 400 instead of catch-all Cursor. That **reduces** accidental Cursor parking, except for IDs that successfully decode to a registered Cursor model — those already would have hit Cursor via the old catch-all, still with the **same** session key (no model in it).

Hypothesis “two public aliases → same decoded model → session collision **because of the patch**”:

- Collision of those two aliases on Cursor park **already existed**, because the key never included model.
- The patch cannot increase **session-key cardinality** for Cursor H2.
- It can only change **which requests reach** the Cursor executor. For 16:43 they already did.

---

## 5. Pre-existing race (high priority, independent)

```go
sessionKey := authID + ":" + conversationId
// restore: take e.sessions[sessionKey]
// park:    publishConversationSession(..., replace=true)  // overwrite
```

No generation id. No pending-set hash in logs. Concurrent:

```
A parks generation 41
B parks generation 42 (overwrite)
A tool_results arrive
restore → generation 42
A IDs ∩ 42 pending = ∅
→ 500 local → cooldown → 503
```

16:43: 107/107 IDs valid in the body, parked pending disjoint, overlapping 200s at 16:44:00 / 16:44:01, retries of the same body until 16:46:21. Code-compatible. Live proof still needs PARK vs RESTORE hashes (pending contents were not logged).

---

## 6. Instrumentation (specified, not applied)

Do this **without** changing match/restore semantics:

On PARK and on MATCH miss, debug log:

- `conversation_id` (already a hash)
- `session_key` suffix (not auth file name if sensitive — authID is a filename)
- `req.Model` (canonical) and inbound model if easily available
- `park_generation` (monotonic per executor or UUID at park)
- `parked_at`
- `pending_count` / `incoming_count`
- `pending_id_sha256[:12][]` / `incoming_id_sha256[:12][]`
- `intersection_count`

If a miss shows `park_source != this request` and `intersection_count=0`, generation overwrite is proven. Still no matcher change.

---

## 7. Concurrent matrix (not run)

A sequential, B parallel same session, C reverse results, D retry, E continue, F subagent, G same public model, H two aliases → same decoded Cursor model.

**H cannot prove a model-ID session-key regression**: aliases already share the Cursor park slot. H would fail **on GOOD and BAD**. If H fails only on BAD, look for a different key than `authID:conversationId` (none found).

The discriminating tests are **B/D/E/F** on a single `session_id`, GOOD vs BAD: expected fail on both if this race is the 16:43 root cause.

---

## Ranking (updated)

| Hypothesis | Assessment |
|---|---|
| Kiro-Go cuts `cursor_call` (16:43) | **DISPROVEN** |
| CLIProxy sanitizer cuts ID (16:43) | **DISPROVEN** |
| Model-ID patch mutates tool IDs | **DISPROVEN** |
| Park key vs restore key use different model strings | **DISPROVEN** |
| Model normalize merges session buckets via model-in-key | **DISPROVEN** (model is not in the key) |
| Pending overwrite / wrong generation restore | **CONFIRMED possible; live hashes UNPROVEN** |
| Model-ID patch as trigger for 16:43 | **LOW** |
| Cursor upstream caused 16:43 | **DISPROVEN** (local matcher, 0 tokens, ~221 ms) |
