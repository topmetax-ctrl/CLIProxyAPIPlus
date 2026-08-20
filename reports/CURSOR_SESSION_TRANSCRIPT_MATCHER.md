# Cursor session matcher — last-turn frontier (P0-K2)

Status: **P0-K2** attempt-aware lifecycle. Last-turn matching remains. Newest-generation fallback is **removed**. See `reports/CURSOR_TOOL_RESULT_LIFECYCLE.md`.

## Symptom (pre-K2 matcher)

Claude Code / Kiro-Go → cliproxy `/v1/messages` returned **400 `TOOL_RESULT_NOT_FOUND`** (or **409 `MIXED_TOOL_RESULT_GENERATIONS`**) when the current tool IDs were exact `cursor_call_…` matches, because the matcher scanned the **whole transcript**.

## Required behavior

Correlate on the **last user turn** only.

Then, for those IDs:

| Frontier hit | Result |
| --- | --- |
| PENDING and/or REPLAYABLE, **one** generation | atomic claim → `IN_FLIGHT` |
| `IN_FLIGHT` (lease still live) | **409 `TOOL_RESULT_IN_FLIGHT`**, no cooldown |
| only `COMMITTED` (active duplicate) | **409 `TOOL_RESULT_ALREADY_CONSUMED`** |
| only `FINAL_REJECTED` | **400 `TOOL_RESULT_FINAL_REJECTED`** |
| two live generations in the last turn | **409 `MIXED_TOOL_RESULT_GENERATIONS`**, **zero mutation** |
| any unknown last-turn ID | **400 `TOOL_RESULT_NOT_FOUND`**, **zero mutation** |
| historical `COMMITTED` IDs **not** in the last turn | ignore |

Retry of `REPLAYABLE` IDs must resolve the **original** generation and **cold-continue** (new H2 stream). Do not resume a finished/stalled stream. Do not fall back to the newest pending generation.

A Cursor keepalive-park (`ServerMsgHeartbeat` on inbound H2) is transport liveness, not a stall. See `reports/CURSOR_STREAM_LIVENESS.md`.

Exact `cursor_call_…` equality stays required; do not prefix-match truncated `cursorcall` IDs.

## Do not “fix” this by

- Requiring the whole transcript to be pending
- Resuming the newest generation when last-turn IDs span two live generations
- Fuzzy-matching truncated `cursorcall` prefixes
- Treating inject-into-Cursor as `COMMITTED`
- Allowing a second upstream attempt while `IN_FLIGHT`

## Related, not this bug

Cursor IDE **WebSearch** (`Did 0 searches in ~800ms`) is a host-product tool. Cliproxy HTTP 200 does not implement or unblock it.
