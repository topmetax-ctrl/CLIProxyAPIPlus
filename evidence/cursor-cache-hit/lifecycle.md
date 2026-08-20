# Cursor TurnEnded lifecycle (P3.5)

```
HTTP /v1/messages or /v1/chat/completions
  → executeStreamOnce / executeOnce
  → EncodeRunRequest + H2 POST /agent.v1.AgentService/Run
  → processH2SessionFrames
  → DecodeAgentServerMessage
  → ServerMsgTurnEnded (InteractionUpdate field 14)
  → observeTurnEnded
  → cursorTokenUsage.settleTurnEnded (exactly once)
  → usage_settled dump + HTTP usage
```

| Event | File | Behavior |
| --- | --- | --- |
| Normal completion | `cursor_executor.go` `processH2SessionFrames` TurnEnded case | settle + return nil + emit usage |
| Tool-call boundary (OpenAI) | same, `openAIToolCallsEmitted` | `NO_TURN_ENDED_EXPECTED` |
| Tool-call park (Claude) | session publish before toolResult wait | `NO_TURN_ENDED_EXPECTED` |
| Tool resume | `resumeWithToolResults` | new `audit_id`, continuity `park_restore` |
| Stream EOF without TurnEnded | data channel close / `stream.Done` | `TERMINAL_USAGE_UNAVAILABLE_BY_PROTOCOL` |
| Cancel | `ctx.Done` | `TERMINAL_USAGE_UNAVAILABLE_BY_PROTOCOL` |
| Watchdog / error | progress timer / stream error | `REAL_FAILURE` |
| Cold flatten | `flattenConversationIntoUserText` | request-shape only; no usage rewrite |
| Connection replacement | stale session cancel | new stream; new audit |

Correlation identity: per-request `audit_id` on fingerprint, TurnEnded dump, and `usage_settled`. Not wall-clock.
