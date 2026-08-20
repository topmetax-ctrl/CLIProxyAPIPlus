# Cursor tool-result lifecycle (P0-K2)

Status: **implemented** in the working tree. Pair with `reports/CURSOR_STREAM_LIVENESS.md` (P0-K1). Do not fold the two into one revert.

The proxy provides **replay-safe local correlation**. After an ambiguous streaming failure it does **not** guarantee exactly-once model execution.

## Why CONSUMED-as-boolean was wrong

Injecting a tool result into Cursor is not a client-visible terminal. The 09:47 incident:

```text
inject succeeded
→ stream ran (thinking + text + heartbeats)
→ 4 minutes later the request was cut (504)
→ client retried the same tool_result IDs
→ 409 TOOL_RESULT_ALREADY_CONSUMED
```

`COMMITTED` is only allowed when **upstream terminal completion was observed** and **the terminal write was delivered downstream**.

## States

```text
PENDING
   ↓ claim
IN_FLIGHT
   ├── terminal success delivered     → COMMITTED
   ├── stream interrupted / 504 / reset / disconnect → REPLAYABLE
   └── final upstream rejection       → FINAL_REJECTED
```

`IN_FLIGHT` is attempt-scoped (`attempt_id`, `claimed_at`, `claim_deadline`). A retry while the original attempt is still `IN_FLIGHT` returns **409 `TOOL_RESULT_IN_FLIGHT`** (request-scoped, no credential cooldown). No second upstream execution.

Stale `IN_FLIGHT` leases recover to `REPLAYABLE` (`cursor_tool_result_stale_claim_recovered_total`). The lease is `cursorSessionHardTTL + 2m`, longer than a normal request.

## Retry correlation

Lookup is **original generation ownership**, never “newest pending generation”.

`REPLAYABLE` is a **cold continuation**: same transcript, same generation id on the result record, **new** H2 stream. Do not restore a stale parked stream. Generation id is correlation, not proof that the old transport is reusable. Do not `retireConversationState` on native Claude cold replay (that would drop sibling generations).

Last-turn / frontier matching stays. Historical `COMMITTED` IDs in the transcript are ignored. Last-turn **known+unknown** and **mixed generations** fail with **zero mutation**.

## Replayable vs final

Replayable (after the attempt ends, still no credential cooldown unless account health is the cause):

- `UPSTREAM_FIRST_BYTE_TIMEOUT`, `UPSTREAM_TRANSPORT_IDLE`, `MAX_STREAM_DURATION_EXCEEDED`
- upstream EOF / reset / network interrupt before terminal
- downstream disconnect before terminal delivery
- request cancellation during a non-terminal stream
- transient 429 / 5xx / connect timeout

`FINAL_REJECTED` is for non-retryable provider rejection (400/401/403/404 class). It must not loop.

## Metrics / logs

```text
cursor_tool_result_claim_total
cursor_tool_result_replay_total
cursor_tool_result_commit_total
cursor_tool_result_inflight_conflict_total
cursor_tool_result_final_reject_total
cursor_tool_result_stale_claim_recovered_total
```

State transitions log `event=cursor_tool_result_state_transition` with `generation_id_hash`, `attempt_id`, `request_id`, `from`, `to`, `reason`. Raw tool-call IDs are not logged.

## Tests

Gate: `TestToolResult_TimeoutThenRetrySameResult_ReplaysOriginalGeneration`.
