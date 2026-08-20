# Cursor stream lifecycle → terminal usage

```
Cursor HTTP/2 DATA
        ↓
H2Stream.readLoop  (complete frames only; DATA END_STREAM closes stream)
        ↓
dataCh / doneCh
        ↓
processH2SessionFrames
        ↓
ParseConnectFrame → InteractionUpdate | Connect END_STREAM
        ↓
TokenDelta | ToolCall (mcpArgs) | Checkpoint | TurnEnded | Error | EOF
        ↓
cursorTokenUsage.settleTurnEnded / addOutput
        ↓
publishCursorUsageSettlement  (after processor returns)
        ↓
OpenAI usage JSON → Claude translator
```

## Frame reader

`http2.Framer.ReadFrame` returns a complete frame or an error. There is no
`n>0 && err==io.EOF` leftover at the Framer. Connect assembler
(`ParseConnectFrame`) holds incomplete **Connect** frames in `buf` until
length is satisfied; a partial Connect frame + EOF does **not** invent
TurnEnded.

`H2Stream.dataCh` is buffered (256). Last DATA can sit in the channel when
`doneCh` closes. `Done()` and `Data()` are both selected; the drain path
must empty `dataCh` and run `processCompleteFrames` before treating the
stream as terminal.

Connect END_STREAM is flag `0x02` with a JSON trailer (`error` optional).
HTTP/2 HEADERS END_STREAM is logged by size only (no dynamic HPACK table,
no usage decode).

## Termination paths

| Path | Expected TurnEnded? | Expected terminal usage? | Current handling |
| --- | ---: | --- | --- |
| Normal completion, TurnEnded then EOF | yes | authoritative five-field usage | settle once; HTTP `stop` |
| TurnEnded already in `dataCh`, `Done()` races | yes | same | **drain then decode** (was drop) |
| TokenDelta then EOF, no TurnEnded | protocol may omit | output partial; input/cache unknown | `eof_without_turn_ended`; no `cached_tokens` |
| Partial Connect frame + EOF | no | none | do not settle |
| Connect END_STREAM without TurnEnded | no | none | `connect_end_seen`; still EOF fallback |
| Tool-call boundary (OpenAI, new request) | **no** | not this HTTP response | `NO_TURN_ENDED_EXPECTED` |
| Tool resume (Claude park) then TurnEnded | yes, after results | authoritative | waitLoop now observes TurnEnded/TokenDelta and drains on Done |
| Tool resume, stream dies while parked | maybe | if TurnEnded already received, use it | drain waitLoop; do not send MCP on dead stream |
| Client cancel | maybe already received | if complete frames in buf/dataCh, settle | drain then `cancelled`; TurnEnded wins classify |
| Watchdog stall | maybe already received | same | drain then 504; do not invent usage |
| Provider Connect error trailer | no | none | `REAL_FAILURE` / stream_error |
| HTTP/2 RST_STREAM / GOAWAY | no | none | stream error |
| Connection reset / io.EOF at Framer | no extra frame | decode whatever already complete | drain + finishOnStreamClose |
| Park / restore ownership change | TurnEnded on the owning stream | must not double-publish | resume rebinds `audit_id`; settle once per usage object |
| Cold continuation (new H2 stream) | per new turn | independent settlement | no flatten change |
| Duplicate TurnEnded | n/a | first wins | `settleTurnEnded` false on second |
| Late TurnEnded after HTTP publish | must not happen | n/a | publish is after processor return |
| Executor return after `tool_calls` | no | TokenDelta output only | not classified as missing TurnEnded |

## Fallback (after processor returns)

```
if TurnEnded settled:
    source = cursor_turn_ended
elif TokenDelta sum > 0:
    source = token_delta   # output only
else:
    source = payload_estimate
```

Never fabricate cache. Never subtract unknown cache from input.
