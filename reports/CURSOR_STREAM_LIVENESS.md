# Cursor H2 stream liveness (P0-K1)

Status: **implemented**. Replay-safe tool-result recovery is **P0-K2** (`reports/CURSOR_TOOL_RESULT_LIFECYCLE.md`).

## 09:47 heartbeats were upstream

The 2026-08-20 09:47 504 logged `heartbeat_since_progress` from `processH2SessionFrames` after `DecodeAgentServerMessage` on **inbound** Connect frames (`stream.Data()`). That decode path sets `ServerMsgHeartbeat` for interaction_update field 13.

Local keepalives are a different object:

| Kind | Direction | Encoder | Watchdog effect |
| --- | --- | --- | --- |
| `UPSTREAM_HEARTBEAT` | Cursor → proxy | `ServerMsgHeartbeat` on `stream.Data()` | resets **transport idle** |
| `LOCAL_KEEPALIVE` | proxy → Cursor | `cursorH2Heartbeat` → `EncodeHeartbeat()` → `stream.Write` | **must not** reset transport idle |
| SSE `: keep-alive` | proxy → Claude Code | `streaming.keepalive-seconds` | downstream only |

So 122 thinking + 31 text + later heartbeats was a live Cursor park, not a dead TCP. The old watchdog measured **semantic** silence and 504'd a false positive.

## Clocks

| Clock | Reset by | On fire |
| --- | --- | --- |
| `last_upstream_frame_at` / transport idle | any inbound H2 bytes (heartbeat, thinking, text, tool, metadata, KV) | 504 `UPSTREAM_FIRST_BYTE_TIMEOUT` or `UPSTREAM_TRANSPORT_IDLE` |
Semantic idle warns **once per episode** (first crossing of the threshold). Thinking/text/tool starts a new episode.
| `max_stream_duration` | not reset; paused while parked for client tool results | 504 `MAX_STREAM_DURATION_EXCEEDED` |

All 504s are **request-scoped** (no credential cooldown).

Config (`cursor:` in YAML, or `CURSOR_TRANSPORT_IDLE_TIMEOUT_S` / `CURSOR_SEMANTIC_IDLE_WARNING_S` / `CURSOR_MAX_STREAM_DURATION_S`):

- `transport-idle-timeout-seconds` default 240
- `semantic-idle-warning-seconds` default 240
- `max-stream-duration-seconds` default 1800; `-1` disables

## Tests

- `TestCursorP0K1_XhighUpstreamHeartbeatParkDoesNotStall` — screenshot pattern
- `TestCursorP0K1_SilentUpstreamIsTransportIdle` — no frames
- `TestCursorP0K1_LocalKeepaliveDoesNotResetTransportIdle` — ClientHeartbeat cannot save a silent upstream
- `TestCursorP0K1_SemanticIdleWarnsWithoutAbort`
- `TestCursorP0K1_MaxDurationAbortsHeartbeatPark`
- `TestCursorP0K1_TransportIdleAfterProgressThenSilence`

## Not P0-K1

Duplicate `TOOL_RESULT_ALREADY_CONSUMED` after a nonterminal 504 is handled in P0-K2: `IN_FLIGHT` / `COMMITTED` / `REPLAYABLE`, retry into the **same** generation, no newest-generation fallback. See `reports/CURSOR_TOOL_RESULT_LIFECYCLE.md`.
