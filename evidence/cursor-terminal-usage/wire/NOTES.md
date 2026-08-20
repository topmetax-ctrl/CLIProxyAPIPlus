# Last frames before EOF (P3.5 capture)

P3.5 `CURSOR_WIRE_DUMP_DIR` wrote `*-turn_ended.json` only when
`TurnEndedUpdate` was decoded. The 7 `eof_without_turn_ended` turns have
**no** `*-turn_ended.json` for their `audit_id`. There is therefore **no**
sanitized last-10 InteractionUpdate hex sequence to decode.

What **is** on disk for each of those turns:

1. `*-fingerprint.json` — request started.
2. `*-usage_settled.json` — `terminal_seen=false`, `usage_source=token_delta`,
   `finish=eof`, TokenDelta `output_tokens` present, cache fields omitted.
3. No Connect END_STREAM dump (that flag was not recorded in P3.5).

Reconstruction (timestamps UTC):

| audit_id prefix | fingerprint | usage_settled | following TurnEnded on session? |
| --- | --- | --- | --- |
| `0e2317a4` CUR-A r1 t1 | 05:39:31.400 | 05:39:35.134 | next turn `2202aa07` TurnEnded 05:39:38.324 |
| `9ac39390` CUR-B r1 t1 | 05:39:38.329 | 05:39:42.866 | next turn `6d25ddfa` TurnEnded 05:39:45.682 |
| `12846ec7` CUR-A r2 t1 | 05:39:51.454 | 05:39:55.588 | next turn `d3805ca6` TurnEnded 05:39:59.035 |
| `616f6b27` CUR-B r2 t1 | 05:39:59.041 | 05:40:03.466 | none for t2 (`278c8d7d` also EOF) |
| `278c8d7d` CUR-B r2 t2 | 05:40:03.471 | 05:40:06.633 | none |
| `46a13d7d` CUR-C r2 t2 | 05:40:10.659 | 05:40:12.907 | none (t1 was tool boundary) |
| `a2ec5657` CUR-A r3 t2 | 05:40:16.515 | 05:40:19.555 | t1 `ad5c2e8c` **had** TurnEnded 05:40:16.508 |

Same-session later turns can still carry TurnEnded. That rejects “this
harness never dumps TurnEnded” and “this conversation never gets TurnEnded”.

Post-fix dumps attach `last_frames` (kind, Connect flags, timestamp; no
payload) on `usage_settled` so a repeat capture can show the sequence
without logging credentials.
