# Cursor Grok prompt-cache audit — 2026-08-20

## PROTOCOL SEMANTICS

`TurnEndedUpdate` (InteractionUpdate field 14) is AUTHORITATIVELY MAPPED
from Cursor.app:

| Field | Semantic |
| ----: | --- |
| 1 | `input_tokens` |
| 2 | `output_tokens` |
| 3 | `cache_read_tokens` |
| 4 | `cache_write_tokens` |
| 5 | `reasoning_tokens` |

Embedded `agent.proto` still lists the message as empty. Names come from
Cursor.app, not this repo’s descriptor.

## USAGE OBSERVABILITY

Terminal TurnEnded settles once into `cursorTokenUsage`, then OpenAI
`cached_tokens` and Claude `cache_read_input_tokens`. Precedence:
TurnEnded > TokenDelta > payloadBytes/4. Absent optional fields stay
unset; present zeros are known zeros. `cache_read > input` is logged and
raw values are preserved.

P3.5 (`p3.5-attribution.jsonl`): 18/18 rows classified. 8
`TURN_ENDED_MATCHED` (wire==internal==HTTP exact). 3
`NO_TURN_ENDED_EXPECTED` (CUR-C t1). 7
`TERMINAL_USAGE_UNAVAILABLE_BY_PROTOCOL` (upstream EOF without
TurnEnded). 0 unknown. Not a harness race.

## WARM CACHE BEHAVIOR

Warm checkpoint continuation preserves cache-read. Typical t2
`cache_read_tokens` 9088–11392. CLIProxy does not destroy Grok cache on
the warm path.

## COLD CACHE BEHAVIOR

True same-semantic A/B (`cold-flatten/`): system/tools/history match;
only continuation implementation differs. HTTP matrix 5 warm + 5 cold
interleaved. Warm t2 uses checkpoint; cold t2 flattens UserText.
TurnEnded-available t2 cache_read: warm 11392/9088/2944/9088 (median
9088), cold 11392/11392/0 (median 11392). Causal degradation: **NO**.
No flatten fix.

Prior CUR-D 2944/6144 was a different conversation (tools + results),
not this A/B.

## CACHE WRITE

Authoritatively mapped. Live non-zero `cache_write_tokens` was **not**
observed (present zeros only).

## REMAINING LIMITATIONS

Terminal reliability (EOF without TurnEnded, last-frame drain, fallback
quality) is tracked in `evidence/cursor-terminal-usage/`, not here.

This file only records cache behavior:

- cache present
- warm preserved
- cold no reproducible degradation
- mapping implemented

Do **not** treat missing TurnEnded as a cache miss.

---

Historical first matrix against instrumented CLIProxy on `127.0.0.1:8321`,
model `cursor-grok-4.6-xhigh-fast`. 36/36 HTTP 200. 3 repetitions.

`cached_tokens` in the HTTP/Claude body is still absent. That is **not**
a miss. Cache-like counters, if they exist, are on `TurnEndedUpdate`
varints (`unknown_field_1..5`). Those fields are **not** named in the
embedded `agent.proto`.

Continuity (checkpoint / park / flatten) is recorded only as a condition.

## Results table

`Cache f3` is `TurnEnded` nested field 3 (cache-read *candidate*).
`null` = TurnEnded dump not attributed for that row (tool-park or harness
race). HTTP cache column is always absent.

| Case | Rep | Turn | HTTP | Continuity | TurnEnded | f1 | f2 | f3 | Result |
| --- | ---: | ---: | ---: | --- | --- | ---: | ---: | ---: | --- |
| CUR-A | 1 | 1 | 200 | fresh | yes | 11397 | 36 | 6144 | UNOBSERVABLE named-cache / PARTIAL_HIT candidate |
| CUR-A | 1 | 2 | 200 | checkpoint | yes | 11490 | 22 | 11392 | HIT candidate |
| CUR-A | 1 | 3 | 200 | checkpoint | yes | 11569 | 22 | 11392 | HIT candidate |
| CUR-A | 2 | 1 | 200 | fresh | yes | 11397 | 36 | 6144 | PARTIAL_HIT candidate |
| CUR-A | 2 | 2 | 200 | checkpoint | yes | 11490 | 22 | 11392 | HIT candidate |
| CUR-A | 2 | 3 | 200 | checkpoint | no | | | | UNOBSERVABLE |
| CUR-A | 3 | 1 | 200 | fresh | no | | | | UNOBSERVABLE |
| CUR-A | 3 | 2 | 200 | checkpoint | yes | 11492 | 37 | 11392 | HIT candidate |
| CUR-A | 3 | 3 | 200 | checkpoint | yes | 11580 | 37 | 11456 | HIT candidate |
| CUR-B | 1 | 1 | 200 | fresh | yes | 11397 | 30 | 6144 | PARTIAL_HIT candidate |
| CUR-B | 1 | 2 | 200 | checkpoint | no | | | | UNOBSERVABLE |
| CUR-B | 1 | 3 | 200 | checkpoint | no | | | | UNOBSERVABLE |
| CUR-B | 2 | 1 | 200 | fresh | yes | 11397 | 36 | 9088 | PARTIAL_HIT candidate |
| CUR-B | 2 | 2 | 200 | checkpoint | yes | 11490 | 20 | 11392 | HIT candidate |
| CUR-B | 2 | 3 | 200 | checkpoint | yes | 11567 | 20 | 11392 | HIT candidate |
| CUR-B | 3 | 1 | 200 | fresh | yes | 11397 | 35 | 9088 | PARTIAL_HIT candidate |
| CUR-B | 3 | 2 | 200 | checkpoint | yes | 11489 | 28 | 11264 | HIT candidate |
| CUR-B | 3 | 3 | 200 | checkpoint | no | | | | UNOBSERVABLE |
| CUR-C | 1 | 1 | 200 | fresh (tool) | no | | | | UNOBSERVABLE |
| CUR-C | 1 | 2 | 200 | park/resume* | no | | | | UNOBSERVABLE |
| CUR-C | 2 | 2 | 200 | park/resume* | yes | 22998 | 84 | 11456 | HIT candidate |
| CUR-C | 3 | 2 | 200 | park/resume* | yes | 22994 | 91 | 22784 | HIT candidate |
| CUR-D | 1 | 2 | 200 | cold_continuation | yes | 11731 | 180 | 2944 | PARTIAL_HIT candidate |
| CUR-D | 2 | 2 | 200 | cold_continuation | yes | 11727 | 176 | 6144 | PARTIAL_HIT candidate |
| CUR-D | 3 | 2 | 200 | cold_continuation | no | | | | UNOBSERVABLE |
| CUR-E | 1 | 2 | 200 | checkpoint, prefix B | yes | 11487 | 22 | 9088 | PARTIAL_HIT candidate |
| CUR-E | 2 | 1 | 200 | fresh, prefix B | yes | 11397 | 36 | 7936 | PARTIAL_HIT candidate |
| CUR-E | 2 | 2 | 200 | checkpoint, prefix B | yes | 11490 | 22 | 9088 | PARTIAL_HIT candidate |
| CUR-E | 3 | 1 | 200 | fresh, prefix B | yes | 11397 | 39 | 6144 | PARTIAL_HIT candidate |
| CUR-E | 3 | 2 | 200 | checkpoint, prefix B | yes | 11493 | 25 | 9088 | PARTIAL_HIT candidate |

\* CUR-C turn 1 is a tool-call boundary: no `TurnEnded` is expected until
after the result is injected on the parked H2 stream. CUR-C turn 2
fingerprint dump is often missing because resume does not rebuild a
request.

XAI DIRECT: **SKIPPED — missing auth** (`0 xAI keys`, no native `grok-*`).
DIRECT Cursor IDE baseline: **UNAVAILABLE**.

## Statistics (field 3 only, observed rows)

| Slice | n | median f3 | median f1 | f3 / f1 if both present |
| --- | ---: | ---: | ---: | --- |
| CUR-A turn 2 | 3 | 11392 | 11490 | 0.991 |
| CUR-B turn 2 | 2 | 11328 | 11490 | 0.987 |
| CUR-E turn 2 | 3 | 9088 | 11490 | 0.791 |
| CUR-D turn 2 | 2 | 4544 | 11729 | 0.387 |
| CUR-A turn 1 | 2 | 6144 | 11397 | 0.539 |

HTTP `prompt_tokens` median on CUR-A is **15554** (payload_bytes/4
estimate). That number is not the wire field 1 value and is not a cache
metric.

## What the old audit got right and wrong

Correct:

- Client HTTP usage has no `cached_tokens`.
- `TokenDeltaUpdate` is field 1 only (`tokens`).
- Checkpoint / park / restore are not cache hits.

Incorrect / incomplete:

- Concluding the **protocol** has no cache usage after inspecting only
  `TokenDeltaUpdate`. Terminal usage is on **`TurnEndedUpdate` field 14**,
  which the old probe never dumped.
- Golden `185-recv.bin` already had five TurnEnded varints
  (`08e2bb0110850218ae9c0120002800`). The descriptor lists
  `TurnEndedUpdate` as empty; the wire does not.

## Field 3 as cache-read candidate

Evidence that **supports** treating field 3 as cache-read:

1. Same conversation, prefix A: t1 median 6144 → t2/t3 11392 (3/3 CUR-A t2).
2. Prefix B (CUR-E) t2 median 9088, not 11392. Prefix hashes differ
   (`70b827d5…` vs `5a966cbd…`). Protocol (Claude vs OpenAI) does not
   explain the drop: CUR-B (OpenAI, prefix A) t2 is 11264–11392.
3. Field 3 ≤ field 1 on every complete row.
4. One fresh frame had field 3 = 0 (`wire/044139.067-0002-turn_ended.json`).

Evidence that **blocks a hard name**:

1. Embedded proto has no field names on `TurnEndedUpdate`.
2. Field 1 does **not** grow when CLIProxy sends a 62 000-byte system
   blob (`system_bytes=62000` in fingerprints). Either Cursor excludes
   that blob from this counter, or this counter is not “all input
   tokens”.
3. Changing A→B at the start of every prefix unit did **not** reset
   field 3 to 0 or to the hidden-only baseline. 9088 is a partial drop.
4. Field 4 was 0 on every observed frame, so “cache write” is unproven.

P1 later closed the name from Cursor.app generated protobuf
(`cache_read_tokens`, field 3). Live differential behavior remains
supporting evidence, not the naming source.

## Did CLIProxy change the stable prefix?

On the encoder side, no accidental mutation of prefix A:

- Every CUR-A/B/D fingerprint with a system hash uses
  `70b827d55f548319fec31eaeaf9b97f7ff1924f9024f52ebaf484aa7069981b2`.
- CUR-E uses `5a966cbd9d78e5225bba7de0b7d23651850fe65af5cb55b15aca5b0b6a86ae9b`.
- `message_id` is a new UUID every request. Field 3 still rose on
  continuation, so that UUID is not a full cache-breaker on this path.

CLIProxy **does** change the *shape* of later turns:

- Warm Claude/OpenAI continuation: `RawCheckpoint` reused (486 bytes on
  CUR-A t2).
- OpenAI tool continuation (CUR-D): `flattenConversationIntoUserText`
  (`cursor_executor.go:1933`) + `cold_continuation`. User text becomes a
  transcript (~779 bytes on CUR-D t2) instead of a checkpoint.

CUR-D t2 field 3 (2944, 6144) is lower than warm t2 (11392). That is a
**candidate** degradation on the cold path only. It is not proof without
a named cache field and a native Cursor baseline.

## Translation layer

```
TurnEnded varints          observed, not mapped
TokenDelta sum             → completion_tokens
payloadBytes/4             → prompt_tokens
CacheReadTokens            never set
prompt_tokens_details      absent
Claude cache_read_*        absent
```

This is an observability gap, not by itself a cache-behavior bug.

## Answers

1. **Does Cursor Grok 4.6 expose cache-read usage on the live protocol?**
   Yes. `TurnEndedUpdate` field 3 is `cache_read_tokens` in Cursor.app
   generated protobuf. Live values match that identity.

2. **Which frame?** `InteractionUpdate.turn_ended` = field **14**.

3. **Does `TokenDeltaUpdate` contain cache usage?** No. Field 1 only.

4. **Was the old TokenDelta probe enough?** No.

5. **CUR-A t2/t3?** Field 3 = 11392 (median). HIT *candidate*.

6. **CUR-B?** Same pattern when TurnEnded was captured. HIT *candidate*.

7. **CUR-C tool continuation?** Two captured t2 rows have field 3 =
   11456 and 22784. HIT *candidate*. Park/restore is not the proof;
   the varints are.

8. **H2 park/restore?** Required for Claude tool resume. Not cache
   proof. CUR-C t1 has no TurnEnded (expected).

9. **Cold vs warm?** CUR-D t2 field 3 median 4544 vs CUR-A t2 11392.
   Lower on cold. Causal file: `flattenConversationIntoUserText` at
   `cursor_executor.go:1933`, gated by
   `coldToolContinuation` at `cursor_executor.go:716-724`. Mapping of
   field 3 is still a candidate, so this is not a shipping bug report.

10. **Does CLIProxy change the stable prefix?** Encoder hashes are
    stable for prefix A. Cold flatten changes *history serialization*,
    not the system-prefix hash.

11. **Does CLIProxy drop cache usage in translation?** Yes: TurnEnded
    varints are not copied into OpenAI/Claude usage.

12. **Does CLIProxy destroy provider prompt cache?** No evidence on the
    warm path. Field 3 rises on continuation through CLIProxy. That is
    not the same as proving field 3 is `cacheReadTokens`, and not a
    native-Cursor comparison.

13. **If degradation:** only the cold OpenAI tool flatten path is a
    candidate (field 3 2944/6144 vs warm 11392), and only if field 3 is
    cache-read. Not a bug report. Do not change flatten yet.

14. **What is proven vs still open?** See the claim table below. Do not
    say “cache behavior is correct” as if field 3 were named.

## Claim table

| Question | Current conclusion |
| --- | --- |
| Cursor Grok has a cache-read-like signal? | **YES — strong evidence** |
| Warm CLIProxy keeps that signal? | **YES — strong evidence** |
| CLIProxy warm path destroys cache? | **NO evidence** |
| Field 3 is definitely `cache_read_tokens`? | **YES — AUTHORITATIVELY MAPPED from Cursor.app** |
| HTTP API exposes cached tokens? | **YES after P2 mapping** (was NO before this change) |
| CLIProxy usage mapping incomplete? | **FIXED in this phase** |
| Cold flatten reduces cache? | **POSSIBLE / not causal** |
| Versus Cursor IDE native | **INCONCLUSIVE** |

```
Cursor Grok prompt-cache HIT:
AUTHORITATIVELY MAPPED on TurnEnded field 3 (`cache_read_tokens`).
Warm continuation reuses a large prefix (median 11392).

CLIProxy warm continuation:
PRESERVED.

CLIProxy destroys cache:
NO EVIDENCE.

HTTP cache observability:
IMPLEMENTED in this phase (terminal TurnEnded → OpenAI/Claude).

Native Cursor comparison:
INCONCLUSIVE.
```

## FINAL VERDICT

Field names are closed from Cursor.app. Cache *behavior* is still
“warm path preserves reuse”; cold flatten remains unproven.

```
CACHE BEHAVIOR:
Warm CLIProxy continuation preserves TurnEnded cache_read_tokens.
No evidence shows CLIProxy destroys prompt caching.

OBSERVABILITY:
P1 AUTHORITATIVELY MAPPED from Cursor.app. P2 maps terminal usage
into cursorTokenUsage and OpenAI/Claude cache fields.

P3 LIVE E2E / P3.5:
PASS. 18/18 rows classified. Matched rows have exact
wire==internal==HTTP. Missing TurnEnded is
TERMINAL_USAGE_UNAVAILABLE_BY_PROTOCOL or NO_TURN_ENDED_EXPECTED.

P4 COLD FLATTEN:
Same-semantic A/B completed. Flatten vs checkpoint does not
reproducibly reduce cache_read. Causal degradation: NO. No flatten fix.

RECOMMENDATION:
NO CACHE-BEHAVIOR CHANGE.
```

Do **not** change checkpoint, park, restore, flatten, routing, or prompt
assembly from this mapping.
