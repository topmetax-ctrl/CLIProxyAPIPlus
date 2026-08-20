# Cursor Semantic Stall Policy

Status: **CANARY ONLY** for a timeout shorter than 240s. request-scoped local watchdog is **PRODUCTION-SAFE**.

## Executive Summary

Dashboard rows that look “slow” on `08d66ec4` are not proxy latency. Healthy turns remain 4–20s. The long rows are `ACTIVE_MODEL` generations that stop emitting thinking/text/tool while Cursor keeps sending `ServerMsgHeartbeat` (~10s). The baseline semantic watchdog treats heartbeats as transport-only and aborts after `cursorNoProgressTimeout`.

A local 504 used to enter conductor transient policy (408/500/502/503/504) and cool the only Cursor credential for 15s, so Claude Code retries became `503 auth_unavailable`. That is fixed by provenance: `cursorWatchdogErr` is HTTP 504 **and** `IsRequestScoped() == true`. Actual Cursor `deadline_exceeded` stays a non-scoped 504.

K1 (`3e5aca53`) must not be cherry-picked: it turns semantic idle into a warning and keeps the stream alive on transport heartbeats until ~30 minutes.

A 60s hard abort is a **canary override**, not a production default. Live healthy semantic gap max was ~12s on 11 streams. That sample cannot prove zero false aborts at 60s. Production default is restored to **240s**. `:8317` may keep `CURSOR_NO_PROGRESS_TIMEOUT_S=60` while shadow telemetry records 30/60/90/120/180 crossings.

## Baseline

- User-smooth baseline: `08d66ec4`
- Branch: `fix/cursor-watchdog-fail-fast`
- Canary binary (pre-shadow): `cliproxy-08d66ec4-faststall` (`Version=08d66ec4-faststall`, 60s compiled)
- Rollback: `cliproxy-08d66ec4-scoped504` (request-scoped 504, 240s compiled)

## Exact K1 Diff

Ancestry: `08d66ec4` is an ancestor of `3e5aca53`. `git diff 08d66ec4..3e5aca53 -- internal/runtime/executor/` **also contains P0-G multi-gen** (`2c5fd235`). Isolated liveness is `cf613243..3e5aca53`.

| | 08d66ec4 | K1 `3e5aca53` |
|---|---|---|
| transport clock | none (single timer) | inbound H2 including heartbeat resets transport idle |
| semantic clock | `progressTimer`; any non-heartbeat resets it | thinking/text/tool reset semantic timer |
| heartbeat | does **not** reset abort timer | resets **transport** clock; does **not** abort on semantic idle |
| tool wait | `waitLoop` does not `select` `progressTimer` | timers stopped around park |
| absolute max | none | `cursorMaxStreamDuration` default ~1800s |
| error type | `cursorStatusErr{504}` | `cursorWatchdogErr(reason)` request-scoped 504 |
| requestScoped | added later on this branch | yes on watchdog/max-duration |
| credential cooldown | 504 was transient (fixed on this branch) | skip cooldown |
| cancel | H2 close / context cancel | same + `markStreamGenerationsFinished` |
| EOF / dead transport | silent stream hits semantic timer | transport idle abort |

K1 T1 (heartbeat forever while ACTIVE) hangs until max duration. That is why K1 is **NO**.

## Existing State Machine

```
ACTIVE_MODEL
  ExecMcpArgs → arm cursorToolBatchIdle
  idle → finalizeToolBatch
    OpenAI (toolResultCh==nil) → emit tools, return
    native → waitLoop (semantic timer NOT selected)
      tool results → send McpResult → resetProgressTimer
      → ACTIVE_AFTER_TOOL_RESULT
```

Proven by source of `processH2SessionFrames` / `finalizeToolBatch` / `waitLoop`. Test: `TestCursorSemanticWatchdogPausedWhileWaitingToolResult`.

## Tool-Wait Semantics

Tool wait is unbounded by design. Shadow observer is paused in `waitLoop` and re-armed after results (`lastSemanticAt` reset so wait time is not a would-abort). Heartbeats during wait update transport only.

## 14 Live Stall Reconstruction

Prior canary log (`/tmp/cliproxy-08d66ec4-scoped504.log`): stalls were `ACTIVE_MODEL`, `pending_batch=0`, `tool_result_ch=true`, last real thinking/text/tool then ~24 heartbeats / ~240s. None were `WAITING_CLIENT_TOOL_RESULT`.

## Local 504 → 503 Root Cause

`wrapStreamResult` → `resultErrorFromError` → `MarkResult`. HTTP 504 matched transient cooldown. Single auth for `cursor-grok-4.6-xhigh-fast` → `503 auth_unavailable`.

## requestScoped Fix

PASS.

- `cursorWatchdogErr` → `cursorStatusErr{code:504, requestScoped:true}`
- `classifyCursorError` returns existing `cursorStatusErr` without stripping provenance
- `resultErrorFromError` sets `Code=request_scoped` via `errors.AsType[RequestScopedError]`, **not** via status==504
- Connect `deadline_exceeded` → `cursorStatusErr{504}` with `requestScoped=false`

Tests: `TestClassifyCursorErrorPreservesWatchdogRequestScoped`, `TestClassifyCursorErrorUpstreamDeadlineIsNotRequestScoped`, `TestLocalCursorWatchdog504DoesNotCooldown`, `TestActualUpstreamCursor504StillCooldowns`.

## Transport vs Semantic Liveness

Two clocks on this branch:

- **Transport:** any inbound frame including heartbeat (`lastTransportAt`)
- **Abort/semantic progress (08d policy):** any **non-heartbeat** resets `progressTimer` (includes TokenDelta/Checkpoint/KV). Heartbeat is not progress.
- **Shadow true-semantic flags:** text / thinking / tool for `has_seen_*` only

Unknown types are counted, not assumed semantic. Abort policy is unchanged from 08d (non-heartbeat resets).

## Shadow Watchdog Design

Observation only. Thresholds 30/60/90/120/180s. Logs only on:

- `cursor_shadow_threshold_cross`
- `cursor_semantic_resume_after_threshold`
- `cursor_stall_terminal`

No prompt/content. Process counters (not Prometheus; repo has none in executor): `cursorStallCounters`.

## Deterministic Tests

Injected clocks (100ms-scale), fake H2 stream.

| Test | Result |
|---|---|
| T1 healthy mix | PASS — no abort |
| T2 / waitLoop pause | PASS — 2× timeout with heartbeats |
| T3 heartbeat-only zombie | PASS — 504 request-scoped, shadow crossed, no resume |
| T4 transport dead (silence) | PASS — same semantic timer |
| T5 shadow false-abort then resume | PASS — `would_abort_50` + `resumed_after_50`, turn ends |

## Live Canary

`:8317` LaunchAgent `com.cursorapi.cliproxy-audit`. After this change, production default is 240s; canary may set `CURSOR_NO_PROGRESS_TIMEOUT_S=60`.

Healthy gap from 2026-08-20 faststall log (11 streams, grok-xhigh-fast only):

- max inter-thinking/text/tool gap: **12s**
- 158s generation, 1258 real events: max gap **8s**
- zombies: full abort window of heartbeats only

**Not** 500 successful turns. **Not** multi-model. Insufficient for default change.

## Healthy Semantic Gap Distribution

| | value |
|---|---|
| n streams | 11 |
| p50 | not computed (second-resolution logs, gaps mostly 0–2s) |
| p90 | ~8s |
| p99 | unknown |
| p99.9 | unknown |
| max | 12s |

## Zombie Distribution

Under 60s canary, zombies abort at ~60s (dashboard 12:18 and 12:24: 60.3–60.7s 504). Under 240s they were ~240s. True-zombie vs false-abort at 60s **cannot** be separated while hard abort is 60s (T5-style resume after 60 never observed live).

## Threshold Comparison

| threshold | would_abort live | false_abort live | true_zombie |
|---|---|---|---|
| 30s | unproven live | 0 in n=11 (max 12s) | likely |
| 60s | canary aborts here | **unproven** (abort cuts observation) | observed |
| 90s | not reached on 60s canary | unknown | unknown |
| 120s | not reached | unknown | unknown |
| 180s | not reached | unknown | unknown |

Rule: candidate eligible iff false_abort_rate=0 **and** threshold > healthy p99.9 × safety_factor **and** tool-wait excluded. **NO-GO** for changing compiled default.

## Retry / Side-Effect Safety

UNPROVEN. Watchdog returns → stream `Close()` / `sessionCancel()`. No Cursor cancel protobuf is sent. Closing H2 does **not** prove the upstream generation dies. Client retry may duplicate tools. Do not shorten production default until this is traced.

## Recommended Default

**240s** compiled. Canary: `CURSOR_NO_PROGRESS_TIMEOUT_S=60`.

## Rollback

1. Point LaunchAgent at `cliproxy-08d66ec4-scoped504` (scoped 504, 240s) or `cliproxy-08d66ec4` (pre-scoped).
2. Or unset `CURSOR_NO_PROGRESS_TIMEOUT_S` on a 240s binary.

## Risks

- 60s canary may false-abort a rare silent-thinking turn not in n=11
- TokenDelta/Checkpoint still reset the abort timer (08d policy)
- Duplicate tool side effects after abort/retry unproven
- K1/K2 remain out of this branch on purpose

## GO / NO-GO

| item | verdict |
|---|---|
| requestScoped 504 | **GO** |
| K1 cherry-pick | **NO-GO** |
| K2 cherry-pick | **NO-GO** |
| compiled default 60s | **NO-GO** |
| shadow telemetry | **GO** (canary) |
| heartbeat = semantic progress | **NO-GO** |
