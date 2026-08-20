# Cursor AgentService protocol — cache/usage audit

Source of schema: embedded `FileDescriptorProto` in
`internal/auth/cursor/proto/descriptor.go` (extracted from
alma-plugins `cursor-auth` `agent_pb.ts`). Field numbers also in
`internal/auth/cursor/proto/fieldnumbers.go`.

Live frames can contain fields the descriptor does not list. Protobuf is
backward-compatible; extra varints are valid. This document records both
the descriptor and the observed wire.

## Call flow (from this repo, not inferred)

```
HTTP /v1/messages or /v1/chat/completions
    → Gin handlers (internal/api)
    → model/provider resolution (cursor auth type)
    → CursorExecutor.ExecuteStream
        internal/runtime/executor/cursor_executor.go:648
    → parseOpenAIRequest / Claude→OpenAI translate
        cursor_executor.go:689-703, 1817
    → deriveConversationId (session_id or system-prompt hash)
        cursor_executor.go:2199
    → checkpoint lookup / flatten / cold continuation
        cursor_executor.go:716-822
    → buildRunRequestParams + EncodeRunRequest
        cursor_executor.go:2050
        internal/auth/cursor/proto/encode.go:110
    → Connect+HTTP/2 POST /agent.v1.AgentService/Run
        cursor_executor.go:39-41
    → processH2SessionFrames
        cursor_executor.go:1460
    → DecodeAgentServerMessage
        internal/auth/cursor/proto/decode.go:78
    → cursorTokenUsage (TokenDelta sum + payload-size estimate)
        cursor_executor.go:1431-1458, 1722-1725
    → OpenAI chunk usage, then Claude translate if needed
        cursor_executor.go:1115-1136
        internal/translator/openai/claude/openai_claude_response.go:336-346
```

Continuity signals (checkpoint, park/restore, flatten, cold continuation)
are **not** prompt-cache proof. They only describe which request path ran.

## AgentServerMessage oneof

| Field | Name | Notes |
| ----: | ---- | ----- |
| 1 | `interaction_update` | `InteractionUpdate` |
| 2 | `exec_server_message` | tools / MCP |
| 3 | `conversation_checkpoint_update` | raw `ConversationStateStructure` |
| 4 | `kv_server_message` | blob get/set |
| 5 | `exec_server_control_message` | |
| 7 | `interaction_query` | |

## InteractionUpdate variants (descriptor)

| Field | Name | Decoder action |
| ----: | ---- | -------------- |
| 1 | `text_delta` | text |
| 2 | `tool_call_started` | ignored |
| 3 | `tool_call_completed` | ignored |
| 4 | `thinking_delta` | thinking text |
| 5 | `thinking_completed` | marker |
| 6 | `user_message_appended` | not specially handled |
| 7 | `partial_tool_call` | not specially handled |
| 8 | `token_delta` | `TokenDeltaUpdate` |
| 9 | `summary` | not specially handled |
| 10 | `summary_started` | not specially handled |
| 11 | `summary_completed` | empty in descriptor |
| 12 | `shell_output_delta` | not specially handled |
| 13 | `heartbeat` | ignore |
| 14 | `turn_ended` | `TurnEndedUpdate` — **terminal usage lives here on the wire** |
| 15 | `tool_call_delta` | not specially handled |
| 16 | `step_started` | ignored |
| 17 | `step_completed` | ignored |

Constants: `IU_TokenDelta = 8`, `IU_TurnEnded = 14` in `fieldnumbers.go`.

## TokenDeltaUpdate

Descriptor:

```
message agent.v1.TokenDeltaUpdate {
  1 optional int32 tokens;
}
```

- InteractionUpdate variant field: **8**
- Nested field 1: incremental token count during the stream
- Live/golden frames: **field 1 only** (example hex `0805` = tokens=5)
- **Does not carry cache usage.** The previous audit that inspected only
  this message could not have seen cache counters.

## TurnEndedUpdate

Descriptor (stale vs live wire):

```
message agent.v1.TurnEndedUpdate {
}
```

Live Grok 4.6 (2026-08-20) and golden composer-2.5 (2026-08-13 frame
`185-recv.bin`) both carry **five varints** on this empty message:

| Nested field | Wire type | Golden composer-2.5 | Live Grok preflight | Live Grok CUR-A t1 | Live Grok CUR-A t2 |
| -----------: | --------- | ------------------: | ------------------: | -----------------: | -----------------: |
| 1 | varint | 24034 | 11392 | 11397 | 11490 |
| 2 | varint | 261 | 25 | 36 | 22 |
| 3 | varint | 20014 | 6144 | 6144 | 11392 |
| 4 | varint | 0 | 0 | 0 | 0 |
| 5 | varint | 0 | 24 | 32 | 18 |

Representative hex:

- Golden `185-recv.bin` TurnEnded payload: `08e2bb0110850218ae9c0120002800`
- Live CUR-A turn 1: `088559102418803020002820` (`wire/CUR-A-turn1-turnended.hex`)
- Live CUR-A turn 2: `08e259101618805920002812` (`wire/CUR-A-turn2-turnended.hex`)

The decoder records these as `TurnEndedFields` and maps present fields
into `TurnEndedUsage`. `cursorTokenUsage` then settles them once into
OpenAI / Claude usage. Absent optional varints stay unset.

### Field semantics

P1 gate: **AUTHORITATIVELY MAPPED** from Cursor.app generated
`agent.v1.TurnEndedUpdate` (see `cursor-app-turnended-schema.txt`).
The embedded alma-plugins descriptor in this repo is still empty.

| Field | Wire type | Semantic            | Evidence                         | Confidence              |
| ----- | --------- | ------------------- | -------------------------------- | ----------------------- |
| 1     | varint    | `input_tokens`      | Cursor.app T:3 field 1; live ~11400 and does not include the 62k KV system blob | confirmed (schema) |
| 2     | varint    | `output_tokens`     | Cursor.app field 2; scales with short vs tool answers | confirmed (schema) |
| 3     | varint    | `cache_read_tokens` | Cursor.app field 3; warm t2 median 11392, prefix-B 9088 | confirmed (schema) |
| 4     | varint    | `cache_write_tokens`| Cursor.app field 4; live value 0 in this capture | confirmed (schema) |
| 5     | varint    | `reasoning_tokens`  | Cursor.app field 5; subset of output on short answers | confirmed (schema) |

Billing was not correlated to the same requests, so this is
`AUTHORITATIVELY MAPPED`, not `CONFIRMED` via account usage.

## Usage field mapping (what CLIProxy actually emits)

```
TurnEndedUpdate
  1 input_tokens          → cursorTokenUsage terminal input
  2 output_tokens         → cursorTokenUsage terminal output
  3 cache_read_tokens     → OpenAI prompt_tokens_details.cached_tokens
                            → Claude cache_read_input_tokens
  4 cache_write_tokens    → OpenAI prompt_tokens_details.cache_write_tokens
                            (when the field is present, including known zero)
  5 reasoning_tokens      → OpenAI completion_tokens_details.reasoning_tokens
TokenDeltaUpdate.tokens   used only if TurnEnded is absent
payloadBytes / 4          used only if TurnEnded input is absent
```

Precedence: terminal TurnEnded > TokenDelta > payload-size heuristic.
Heuristic writes are ignored after terminal settle. Absent optional
fields stay unset; present zeros are known zeros.
`usage_source=cursor_turn_ended|token_delta|payload_estimate` is dumped
on `usage_settled` and correlated by per-request `audit_id`.

The OpenAI→Claude translator subtracts `cached_tokens` from
`prompt_tokens` only when `cache_read > 0` and `input >= cache_read`.
If `cache_read > input`, raw values are preserved and an anomaly is
logged — no wrap, no invented zero. Known-zero `cache_read` is emitted
as Claude `cache_read_input_tokens: 0`.

## Request serialization notes (prefix / cache conditions)

- System prompt is stored as a SHA-256-addressed KV blob
  (`encode.go:154-164`), not inline in `UserMessage`.
- Every request gets a new `message_id` UUID
  (`cursor_executor.go:2062`). This did **not** prevent field 3 from
  rising on turn 2.
- OpenAI tool-result path flattens history into `UserText` and retires
  checkpoints (`cursor_executor.go:716-724`, `1933`).
- Claude tool-result path parks the H2 stream and resumes
  (`cursor_executor.go:726-754`).
- `deriveConversationId` prefers Claude `metadata.user_id.session_id`
  (`cursor_executor.go:2199`).

## Unknown / unused descriptor types

No `TokenUsage` or `Usage` message exists in the embedded descriptor.
`ConversationTokenDetails` is `{used_tokens, max_tokens}` on
conversation state (context window), not turn billing.

The embedded descriptor still lists `TurnEndedUpdate` as empty. Field
names come from Cursor.app generated `agent.v1.TurnEndedUpdate`, not
from this repo’s `descriptor.go`.
