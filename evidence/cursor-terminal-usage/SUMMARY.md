# Cursor terminal usage — SUMMARY

Audit date: 2026-08-20. Model: `cursor-grok-4.6-xhigh-fast`. Source capture:
`evidence/cursor-cache-hit/p3.5-attribution.jsonl` (18 logical turns) plus
Cursor.app generated `agent.v1` schema.

Cache behavior is **not** reopened here. See `evidence/cursor-cache-hit/SUMMARY.md`.

## P3.5 classification (pre-drain-fix capture)

```
TURN_ENDED_MATCHED:                 8
NO_TURN_ENDED_EXPECTED:             3   (CUR-C turn 1 tool boundary)
EOF_WITHOUT_TURN_ENDED:             7
REAL_FAILURE:                       0
UNKNOWN:                            0
```

The 7 EOF cases are **not** "Cursor always omits TurnEnded". Same sessions
often have TurnEnded on the next turn. They are also **not** tool boundaries.

| case | rep | turn | continuity | TokenDelta output | TurnEnded dump |
| --- | ---: | ---: | --- | ---: | --- |
| CUR-A | 1 | 1 | fresh | 12 | none for this audit_id |
| CUR-B | 1 | 1 | fresh | 16 | none |
| CUR-A | 2 | 1 | fresh | 12 | none |
| CUR-B | 2 | 1 | fresh | 12 | none |
| CUR-B | 2 | 2 | checkpoint | 11 | none |
| CUR-C | 2 | 2 | park_restore | 67 | none |
| CUR-A | 3 | 2 | checkpoint | 10 | none |

HTTP 200 on all 7. TokenDelta was observed. Cache fields were absent (not
zero). Estimated `prompt_tokens` (~15554) is **not** copied into
`usage.input_tokens` in `results.jsonl`.

## Root cause

Two facts are both true:

1. **Lost-final-frame bug (code): YES.** `H2Stream.dataCh` is buffered (256).
   `readLoop` sends the last DATA then closes `dataCh` then `doneCh`.
   `processH2SessionFrames` `select`s both. If `Done()` wins, leftover DATA
   (often the TurnEnded Connect frame) was previously returned without
   draining. This is not `n>0 && io.EOF` leftover: `http2.Framer.ReadFrame`
   only returns complete frames. The race is **Done vs unread Data**.

2. **Live 7/18 as that race: PARTIAL.** Those captures have no TurnEnded dump
   for the `audit_id`, which is consistent with either the race **or** Cursor
   omitting TurnEnded. P3.5 dumps were TurnEnded-gated, so last-10
   InteractionUpdate hex is unavailable for those turns.

After the drain fix, complete frames already in `dataCh` / `buf` are decoded
before cancel, watchdog, park waitLoop, and stream `Done()` terminal handling.

## Alternate authoritative terminal source

None on `AgentService/Run`.

| Message | Fields | Live on Run? | Terminal billing? | Fallback? |
| --- | --- | ---: | ---: | --- |
| `TurnEndedUpdate` | input, output, cache_read, cache_write, reasoning | yes | **yes** | primary |
| `TokenDeltaUpdate` | `tokens` (INT32) | yes | no — incremental, incomplete vs TurnEnded output | output only |
| Connect END_STREAM JSON | `error.code` / `error.message` | yes | no usage | error / stream end flag only |
| HTTP/2 HEADERS END_STREAM | HPACK fragment, not decoded | possible trailers | no usage observed | no |
| `ConversationTokenDetails` | used_tokens, max_tokens, breakdown | conversation state, not Run terminal | context window | no |
| `PromptTokenBreakdown*` / `GetPromptContextUsage*` | estimated context tree | separate RPC | no | no |
| `AfterAgentResponseRequestQuery` / `StopRequestQuery` | input/output/cache_* | **not** an `InteractionQuery` oneof on Run | IDE hook, not Run stream | no without live Run evidence |
| `InteractionUpdate.summary*` | descriptor only | not dumped on these captures | not proven | no |

Connect trailers are JSON `{"error":...}` (`ParseConnectEndStream`). No
`grpc-status` usage metadata and no cache counters.

## TokenDelta semantics

Proven:

- Nested field 1 = `tokens`.
- Summed during the stream into `cursorTokenUsage.outputTokens`.
- When TurnEnded is present, TurnEnded `output_tokens` **replaces** the sum
  (example: golden/live streams where deltas 5+4+1+1=11 vs TurnEnded output 22).
- Never contains cache or input.

Therefore TokenDelta is **partial observed output**, not complete billed
output, and is never used to estimate input or cache.

## Fallback hierarchy

1. `cursor_turn_ended` — all five fields when present; absent optional fields stay unset.
2. `token_delta` — output only; input unknown; cache unknown.
3. `payload_estimate` — input ≈ payloadBytes/4 for OpenAI `prompt_tokens` compatibility; output 0 if no deltas; cache unknown.

HTTP OpenAI still emits `prompt_tokens` from the estimate when TurnEnded
input is missing (existing schema). It does **not** emit
`prompt_tokens_details.cached_tokens`. Claude does not emit
`cache_read_input_tokens` unless cache is known.

## Usage quality

| Source | Input | Output | Cache read | Cache write |
| --- | --- | --- | --- | --- |
| TurnEnded | authoritative | authoritative | authoritative if field present | authoritative if field present |
| TokenDelta only | unknown (HTTP may still show estimate) | partial observed | unknown | unknown |
| Estimate only | estimated | estimated/0 | unknown | unknown |

Present-zero vs absent is preserved for TurnEnded optional fields.

## Exactly-once

Settlement into `cursorTokenUsage` is once (`settleTurnEnded` ignores
duplicates). HTTP/Claude mapping is published after the stream processor
returns, so a drained late TurnEnded still wins before publish. Fallback is
not published early.

## Observability

Process-local counters on `GET /v0/management/usage` → `cursor_usage`:

- `cursor_usage_source_turn_ended` / `_token_delta` / `_estimate`
- `cursor_turnended_missing_eof` / `_tool_boundary` / `_cancel` / `_error`
- `cursor_cache_usage_observable_total` / `_unobservable_total`
- `cursor_cache_read_tokens_total` / `cursor_cache_write_tokens_total` (known fields only)
- `cursor_usage_protocol_anomaly_total` (`cache_read > input`)

No conversation_id / request_id / email labels. `usage_settled` dumps include
`last_frames` (kind/flags/timestamp only) when `CURSOR_WIRE_DUMP_DIR` is set.

## Cache write

Protocol and mapping support `cache_write_tokens`. Live non-zero still **not**
observed (present zeros on TurnEnded).

```
CACHE WRITE:
SUPPORTED BY PROTOCOL/MAPPING
LIVE NON-ZERO NOT OBSERVED
```

## P15 live matrix (drain-fix binary on 127.0.0.1:8325)

35 classified turns. 0 unknown. 0 invented cache. 0 `eof_without_turn_ended`.

| Slice | n | Notes |
| --- | ---: | --- |
| normal Claude | 5 | all `TURN_ENDED_MATCHED` |
| normal OpenAI | 5 | all `TURN_ENDED_MATCHED` |
| warm t1+t2 | 10 | all matched; t2 cache_read 9088/2944/9088/2944/11392; HTTP cached exact |
| tool t1 | 3 | `NO_TURN_ENDED_EXPECTED` |
| resume t2 | 3 | all matched; cache_read 20480/17024/20480 |
| cancel | 2 | client timeout; `cancelled`; no cache field |
| cold t2 | 3 | all matched; cache_read 0/11264/11392 (not a flatten experiment) |
| long | 1 | matched, output 57 |

TurnEnded coverage this run: 23/26 successful 200 non-tool-boundary turns in the first 28, then 4/4 added cold/long = **high**. The previous 7/18 EOF cluster did **not** reproduce after draining leftover DATA.

Warm cache smoke: 5/5 t2 `cache_read_tokens > 0` and OpenAI/Claude mapping exact. No checkpoint/flatten change.

Last-frame kinds on tool boundary (no TurnEnded, as expected):
`text/token/tool/kv/checkpoint` plus unnamed `type_0` (non-TurnEnded InteractionUpdate variants). Not decoded as usage.

## Production fix

**YES** — drain leftover H2 DATA before EOF/cancel/watchdog/park Done
handling. Not a checkpoint/flatten/cache-behavior change.
