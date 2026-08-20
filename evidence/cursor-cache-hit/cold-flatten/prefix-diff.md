# P4 first prefix divergence

Same account, model `cursor-grok-4.6-xhigh-fast`, system sha
`70b827d55f548319fec31eaeaf9b97f7ff1924f9024f52ebaf484aa7069981b2`,
same unused `echo_mock` schema, same user turns. Only continuation
implementation differs on turn 2. HTTP matrix: 5 warm + 5 cold interleaved.

| Layer | Warm (A t2) | Cold (B t2) |
| --- | --- | --- |
| system | identical sha | identical sha |
| tools | identical sha `2b169f72…` | identical sha |
| reasoning / model | same | same |
| **history / user_text** | short follow-up `PONG-2` (`a09d6ffa…`) | flattened transcript (`983cc65b…`) |
| checkpoint | present (`has_raw_checkpoint=true`) | absent |
| continuity | `checkpoint` | `flatten` |

First prefix-relevant divergence: **history serialization** (checkpoint +
short UserText vs `flattenConversationIntoUserText` transcript).

Expected cache consequence if flatten broke reusable prefix: cold
`cache_read_tokens` would be reproducibly lower. Observed TurnEnded t2
values:

- warm: 11392, 9088, 2944, 9088
- cold: 11392, 11392, 0

No reproducible reduction. Causal degradation: **NO**. P5 not required.
