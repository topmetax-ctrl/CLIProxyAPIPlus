# Cursor terminal usage reliability

This directory is the accounting/lifecycle audit. Cache *behavior* lives in
`evidence/cursor-cache-hit/` and is closed: Grok prompt cache is present,
warm CLIProxy preserves it, cold flatten did not reproducibly degrade it.

Do not mix the two questions.

## What this audit answers

1. Why some logical turns end as `eof_without_turn_ended`.
2. Whether a last-frame / `Done()` vs `Data()` race dropped `TurnEnded`.
3. Whether any other Cursor protocol message is an authoritative terminal
   usage source on `AgentService/Run`.
4. How fallback must behave when `TurnEnded` is absent.

## Files

| Path | Contents |
| --- | --- |
| `SUMMARY.md` | Verdict, fallback hierarchy, remaining unknowns |
| `lifecycle.md` | Stream termination paths and expected usage |
| `results.jsonl` | P3.5 logical turns classified per protocol/lifecycle |
| `wire/NOTES.md` | Why last-10 hex dumps were unavailable; reconstruction |
| `fixtures/` | Sanitized settlement copies for the 7 EOF cases |

## Classification rules

- Tool-call boundary (`finish=tool_calls`) is **not** missing TurnEnded.
- `TurnEnded` present → `TURN_ENDED_MATCHED`, even if cancel/EOF follows.
- Stream EOF without TurnEnded → `EOF_WITHOUT_TURN_ENDED`.
- Unknown cache is omitted, never emitted as `0`.
- TokenDelta is never used for input or cache.
