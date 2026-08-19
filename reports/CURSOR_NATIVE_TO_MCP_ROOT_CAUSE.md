# Cursor native-tool → reject → MCP fallback: root-cause audit (Phase A)

Status: **Phase A complete. Root cause proven. GO for implementation = NO for a
protocol/capability fix; GO = YES only for a bounded rejection-semantics
mitigation (see Recommendation).**

All conclusions below are backed by live runs against the real Cursor
AgentService on the same account, model, prompt, workspace and tool set. No
conclusion in this document rests on source reading alone.

---

## Executive summary

When a request goes through CLIProxy's Cursor direct provider, the upstream
Cursor agent asks the client to execute **native** tools (`glob`/`grep`,
`read`, `ls`, `shell`) before it touches any MCP tool. CLIProxy has no
workspace and no filesystem for the caller's project, so it answers every one
of those with a `*Rejected` message. The model then narrates something like
"Cursor tools aren't available in this session, switching to MCP" and re-issues
the work as an MCP call.

The audit set out to find the missing capability/field that would stop Cursor
from asking. **That field does not exist.** The same native-first ordering was
reproduced on the official `cursor-agent` binary, on the same account and
prompt, in every configuration tested — including with MCP tools registered and
with ACP client capabilities explicitly declaring `fs.readTextFile = false`,
`fs.writeTextFile = false`, `terminal = false`.

Cursor's private AgentService has **no client capability negotiation for native
exec tools**. The server-side harness always advertises its native tool suite;
the protocol's `ReadRejected` / `ShellRejected` / `WriteRejected` / … messages
are the *designed* channel for a client that cannot or will not run a tool.
CLIProxy is already using the protocol correctly.

What is left is not a protocol defect but a cost: ~5–6 wasted native attempts
per turn, one wasted reasoning cycle, and a user-visible apology from the model.
Only the last of those is worth acting on, and only through the rejection text.

---

## Versions tested

| Item | Value |
| --- | --- |
| Tested HEAD | `5ecb91a37c5daeb5de31713e7863c509e59ac477` (`audit/cursor-live-validation`) |
| Baseline | `4823235a` (tag `v7.2.127-3`, `cursor-audit-baseline-2026-08-13`, `origin/main`) |
| Official Cursor Agent | `2026.08.11-e8db854` |
| Upstream | `https://api2.cursor.sh`, `/agent.v1.AgentService/Run` (BiDi stream) |
| Models tested | `cursor-grok-4.6-xhigh-fast`, `claude-4.5-sonnet`, `composer-2.5` |
| Account | single Cursor OAuth account, identical across every arm |
| Workspace fixture | `/tmp/cursor-tool-audit-proj` (`package.json` → `zephyr-quokka-ledger`) |

Worktrees used: `worktrees/baseline-4823235a`, `worktrees/current-head`.
The main working tree was never reset and holds only audit tooling
(`cmd/cursorprotodump`, `cmd/cursorcapture/relay`, `evidence/`, `reports/`).

---

## Reproduction

Deterministic. One OpenAI-compatible request with two client tools
(`read_file`, `list_dir`) and the prompt:

> Read the package metadata from the current project and report the
> package/project name. Use the available tools rather than guessing.

Harness: `evidence/cursor-tool-runtime/run_probe.py`. It classifies the first
tool decision of each run by slicing the proxy debug log for that run and
mapping `cursor: decoded server message type=<n>` onto the
`ServerMessageType` iota in `internal/auth/cursor/proto/decode.go`.

---

## Baseline vs current

| Arm | Runs | Native-first | MCP-first | No tool |
| --- | ---: | ---: | ---: | ---: |
| baseline `4823235a`, grok-4.6-xhigh-fast | 5 | **5** | 0 | 0 |
| current HEAD `5ecb91a3`, grok-4.6-xhigh-fast | 5 | **5** | 0 | 0 |

Byte-identical event sequences on both arms. The `rejectReason` string
(`"Tool not available in this environment. Use the MCP tools provided
instead."`) is character-identical at both commits and predates both
(introduced in `19c52bcb`).

> **NOT A REGRESSION FROM THE GROK CLOAK/CANONICAL FIX.**

Raw data: `evidence/cursor-tool-runtime/runs-baseline-grok.jsonl`,
`runs-head-grok.jsonl`.

---

## Event trace

Real timeline from `/tmp/audit-head.log`, one turn:

```
14:35:33  type=6   ExecRequestCtx      server asks for context
14:35:33           -> CLIProxy replies RequestContextResult{ tools:[read_file, list_dir] }
14:35:35  type=13  ExecGrepArgs        native grep  #1  -> GrepError(rejectReason)
14:35:35  type=13  ExecGrepArgs        native grep  #2  -> GrepError(rejectReason)
14:35:35  type=13  ExecGrepArgs        native grep  #3  -> GrepError(rejectReason)
14:35:35  type=13  ExecGrepArgs        native grep  #4  -> GrepError(rejectReason)
14:35:35  type=13  ExecGrepArgs        native grep  #5  -> GrepError(rejectReason)
14:35:35  type=7   ExecMcpArgs         toolName=list_dir  -> surfaced to client
                                       finish_reason=tool_calls
```

Two facts this timeline settles:

1. `ExecRequestCtx` (with the full MCP tool list) is answered **two seconds
   before** the first native attempt, and `AgentRunRequest.mcp_tools` carried
   the same list in the initial request. MCP availability is not late.
2. The rejections do not break the turn. Every run recovered and produced a
   correct `tool_calls` response. Issue B (parallel tool calls) never
   manifested here and is kept out of scope.

---

## Current direct-provider architecture

Source paths:

- Request build: `internal/auth/cursor/proto/encode.go` → `EncodeRunRequest`
  (line 110) and `encodeRunRequestWithCheckpoint` (line 248).
- Request-context reply: `EncodeExecRequestContextResult` (encode.go line 440).
- Native rejection: `internal/runtime/executor/cursor_executor.go`,
  `processH2SessionFrames`, lines 1763–1782.

Descriptor ground truth was produced with `cmd/cursorprotodump` from the
embedded `agent.proto` FileDescriptorProto
(`evidence/cursor-tool-runtime/agent-proto-descriptor.txt`, 492 messages).

### Capability inventory

| Capability | Descriptor has field? | Encoder sets it? | Runtime handles it? |
| --- | --- | --- | --- |
| MCP tools | yes — `AgentRunRequest.mcp_tools` (4), `RequestContext.tools` (7) | **yes, both** | yes |
| filesystem read | no capability field; only `ExecServerMessage.read_args` | n/a | rejects (`ReadRejected`) |
| filesystem write | same | n/a | rejects (`WriteRejected`) |
| shell | same | n/a | rejects (`ShellRejected`) |
| terminal | no field | n/a | rejects |
| workspace | `RequestContext.env.workspace_paths` (4→2), `ConversationStateStructure.previous_workspace_uris` (9) | **no** | n/a |
| cwd | `RequestContextEnv.project_folder` (11), `process_working_directory` (21) | **no** | n/a |
| permissions | no client-declared field; only server→client `*PermissionDenied` results | n/a | n/a |
| sandbox | `RequestContextEnv.sandbox_enabled` (5), `SandboxPolicy` on exec args | **no** | ignored |
| execution environment | `RequestContext.env` (4), `AgentRunRequest.harness` (13) | **no** | n/a |

**There is no `capability` field anywhere in the protocol.** A keyword scan of
all 492 messages for `capabilit`, `execution_mode`, `is_remote`, `headless`
returns zero hits. That is the single most important structural finding.

### Fields present but dropped by the encoder

`AgentRunRequest`: `mcp_file_system_options` (6), `skill_options` (7),
`custom_system_prompt` (8), `requested_model` (9).

`RequestContext`: `rules` (2), `env` (4), `repository_info` (6),
`conversation_notes_listing` (8), `shared_notes_listing` (9), `git_repos` (11),
`project_layouts` (13), `mcp_instructions` (14), `debug_mode_config` (15),
`cloud_rule` (16), `web_search_enabled` (17), `skill_options` (18),
`file_contents` (20), `user_intent_summary` (21), `custom_subagents` (22),
`mcp_file_system_options` (23).

> FIELD EXISTS / ENCODER DROPS IT — recorded, then differentially tested below.
> Every one of the plausible candidates was tested and none is causal.

### Descriptor drift

The bundled descriptor is **stale but not wrong**. Official
`AgentRunRequest` at 2026.08.11 has 28 fields; the bundled copy has 9. Fields
1–9 are identical in number, name and type. The bundled copy is missing
`harness` (13), `exclude_workspace_context` (12), `run_id` (25),
`agent_session_id` (26) and four `client_supports_*` booleans (19, 23, 27, 28).
None of the missing fields is a filesystem/terminal capability.

---

## Official ACP control

`cursor-agent acp` driven by `evidence/cursor-tool-runtime/acp_probe.py`,
same workspace and prompt.

| Arm | `fs.readTextFile` / `writeTextFile` | `terminal` | First tool kinds observed |
| --- | --- | --- | --- |
| ACP-A | `false` | `false` | `search/Find` ×5, then `other/MCP: tool` |
| ACP-B | `true` | `true` | `search/Find` ×5 and `other/MCP: tool` |

**Identical.** Declaring no filesystem and no terminal did not stop the agent
from selecting native search tools first.

Caveat stated honestly: in ACP the agent is a local process that owns a real
filesystem, so `clientCapabilities.fs` only chooses *how* it reads, not
*whether* it can. ACP is therefore a weaker control than the direct-provider
comparison below, and it is not treated as decisive on its own. Per the audit
rules, no ACP field was copied into the private protobuf.

Raw data: `evidence/cursor-tool-runtime/acp-A-nofs.json`, `acp-B-fs.json`.

---

## Official SDK control

From `cursor/sdk-bridge` `proto/sdk/v1/sdk_messages.proto` (fetched at audit
time) and `docs/`:

| Concept | Official SDK | CLIProxy direct |
| --- | --- | --- |
| Workspace location | `LocalAgentOptions.cwd` (required for local agents), `dirs` for multi-root; docs state `dirs` makes "request-context workspace metadata cover every entry" | never sent |
| Host-owned custom tools | `LocalAgentOptions.custom_tools` declared **at agent creation**; execution via `SdkCustomToolCallbackService.CallCustomTool` | client `tools[]` mapped to `McpToolDefinition`, sent per request on `AgentRunRequest.mcp_tools` *and* `RequestContext.tools` |
| MCP server tools | `SendOptions.mcp_servers` | same channel as custom tools |
| Native filesystem | implied and implemented by the bridge host | not implemented; rejected |
| Sandbox | `SandboxOptions.enabled` | not sent |
| Tool callbacks | adapter-side loopback Connect server | inline on the BiDi stream |

The SDK bridge is used here strictly as a behaviour/architecture oracle. Its
API-key auth model is deliberately not mixed into the OAuth direct-provider
question.

Relevant divergence: official hosts declare custom tools at **agent creation**;
CLIProxy declares them per request. Tested below (H3) and found non-causal —
CLIProxy already sends the tools in the initial `AgentRunRequest`, so they are
present before the first decision.

---

## Differential request analysis

A recording pass-through (`cmd/cursorcapture/relay`) was built to capture the
official client's own traffic legitimately: `cursor-agent --endpoint
http://127.0.0.1:8501` forwards to `https://api2.cursor.sh` byte-for-byte, with
credential headers recorded only as `<redacted len=N>`. No TLS interception, no
re-signing, no header rewriting.

76 official RPCs were captured. Only the sanitized inventory is kept in the
repository (`evidence/cursor-tool-runtime/official-relay-rpcs.jsonl`); the raw
response bodies are deliberately not committed because they contain the test
account's identity. `AgentService/Run`
itself does **not** honour `--endpoint` and never reached the relay, so the
request-level diff was completed from primary source instead: the official
client's own `AgentRunRequest` constructor and protobuf-es field tables inside
`index.js` of the shipped binary.

Official constructor (verbatim field list):

```
conversationState, action, modelDetails, requestedModel, mcpTools,
conversationGroupId, conversationId, agentSessionId, mcpFileSystemOptions,
customSystemPrompt, harness, computerUseCoordinateMode, suggestNextPrompt,
subagentTypeName, excludeWorkspaceContext, devRawModelSlug,
selectedSubagentModels, subagentModelOverrides, selectedSubagentModelDetails,
clientSupportsInlineImages, clientSupportsSendToUser,
clientSupportsPromptContextUsageRpc, clientSupportsRoutedModelUpdate,
canCreateCloudSubagents, suppressSubagentProgressUpdateTool, runId
```

CLIProxy sends `conversation_state`, `action`, `model_details`, `mcp_tools`,
`conversation_id`. The official client additionally fills `RequestContextEnv`
with `osVersion`, `workspacePaths`, `shell`, `sandboxEnabled`, `projectFolder`,
`terminalsFolder`, `timeZone`, `processWorkingDirectory`; CLIProxy sends none.

Each of these gaps was then tested rather than assumed.

---

## Hypothesis matrix

### H1 — missing execution capability signalling

- Supporting: `RequestContext.env`, `mcp_file_system_options` and `harness` are
  all droppable/dropped; official clients set them.
- Contradicting: no `capability` field exists in any of the 492 messages;
  official ACP with `fs=false, terminal=false` behaves identically to
  `fs=true, terminal=true`; setting `mcp_file_system_options.enabled` to
  `false` **and** `true` both left the native-first rate unchanged at 4/5.
- Test: `evidence/cursor-tool-runtime/run_experiments.sh`, arms `mcpfs-off`,
  `mcpfs-on`, plus ACP-A/ACP-B.
- Result: `control` 4/5 native-first · `mcpfs-off` 4/5 · `mcpfs-on` 4/5.
- **Verdict: DISPROVEN.** The capability does not exist to be signalled.

### H2 — missing workspace/environment metadata

- Supporting: `RequestContext.env` is never populated; official always
  populates it. Arm `rcenv` (`os_version="remote-gateway"`, `shell="none"`,
  no `workspace_paths`) moved first-decision from 4/5 native to 2/5 native,
  3/5 MCP.
- Contradicting: total native exec attempts were **unchanged** (30 events over
  5 runs vs 27 in control). The env only reordered the first pick; it did not
  reduce native attempts. Effect is non-deterministic and within run-to-run
  noise for a 5-run sample.
- Test: arm `rcenv`.
- **Verdict: WEAK / NOT CAUSAL.** Influences ordering, does not suppress native
  tool advertisement. Populating it with synthetic values would also be
  dishonest telemetry.

### H3 — MCP tools arrive too late

- Test: event timeline plus the `AgentRunRequest.mcp_tools` payload.
- Result: MCP tools are in the initial run request, and `ExecRequestCtx` is
  answered at 14:35:33 versus the first native attempt at 14:35:35.
- **Verdict: DISPROVEN.**

### H4 — MCP schemas/naming inadequate

- Test: `runs-head-grok-notools.jsonl` — same prompt with `tools` omitted
  entirely.
- Result: 2/5 still native-first, 20 native exec events across 4 runs, with
  **zero** MCP tools declared.
- **Verdict: DISPROVEN.** Native tools are advertised independently of whether
  any MCP tool exists.

### H5 — Cursor harness/system prompt prefers native tools

- Supporting: an explicit user-level system prompt stating that native tools do
  not exist and will be rejected changed nothing (5/5 native-first). Setting
  `AgentRunRequest.custom_system_prompt` was rejected by the server with
  `invalid_argument: unknown option '--system-prompt'`, which shows the server
  materialises a CLI-style harness with a fixed option set. `harness` (13) is
  absent from the bundled descriptor and is not set by the official CLI either.
- Contradicting: none.
- **Verdict: CONFIRMED as the mechanism.** The server-side harness owns the
  tool list, and it is not steerable from the client on this account/harness.

### H6 — model-specific (Grok only)

- Test: three models, same prompt/tools/account.
- Result: grok-4.6-xhigh-fast 5/5, claude-4.5-sonnet 4/4, composer-2.5 4/4 —
  all native-first.

  | Model | Native first | MCP direct | Recovered after reject |
  | --- | ---: | ---: | ---: |
  | `cursor-grok-4.6-xhigh-fast` | 5/5 | 0/5 | 5/5 |
  | `claude-4.5-sonnet` | 4/4 | 0/4 | 3/3 completed runs |
  | `composer-2.5` | 4/4 | 0/4 | 4/4 |

- **Verdict: DISPROVEN.**

### H7 — protocol/client-version drift

- Supporting: bundled descriptor has 9 `AgentRunRequest` fields vs 28 official.
- Contradicting: overlapping fields 1–9 match exactly; none of the 19 missing
  fields is a filesystem/terminal capability; the official client at the
  current version exhibits the *same* native-first behaviour.
- **Verdict: REAL BUT NOT CAUSAL.** Worth refreshing as hygiene; it does not
  explain the symptom.

### H8 — regression from the recent Grok/model-ID patch

- Test: baseline `4823235a` vs HEAD `5ecb91a3`, 5 runs each.
- Result: 5/5 native-first on both, identical sequences.
- **Verdict: DISPROVEN.**

### H9 — expected behaviour by design

- Supporting, and decisive: the official `cursor-agent`, same account, model,
  prompt and workspace, reports its own tool calls as

  - without MCP: `globToolCall` ×4 → `readToolCall`
  - with an MCP server registered offering `read_file`/`list_dir`:
    `globToolCall` ×2 → `getMcpToolsToolCall` → `mcpToolCall`

  The official client is native-first too. The protocol ships
  `ReadRejected` / `ShellRejected` / `WriteRejected` / `LsRejected` /
  `*PermissionDenied` precisely so a client can decline.
- **Verdict: CONFIRMED.**

---

## Proven root cause

**Cursor's private AgentService performs no client capability negotiation for
native execution tools. The server-side harness unconditionally advertises its
native tool suite (glob/grep, read, write, ls, shell) to the model, and the
model selects them first regardless of client type, model family, MCP tool
availability, MCP tool naming, ACP capability declarations, or user system
prompt. Declining a native tool with the protocol's `*Rejected` result is the
designed client-side mechanism. CLIProxy is a remote gateway with no access to
the caller's workspace, so it must decline, and the model's fallback to MCP —
including the visible "Cursor tool unavailable, switching to MCP" narration —
is the correct and expected consequence.**

Evidence:

1. **Official-client control, no MCP.** `cursor-agent 2026.08.11-e8db854`,
   `cursor-grok-4.6-xhigh-fast`, identical prompt/workspace/account:
   `globToolCall` ×4 then `readToolCall`. Native-first with zero MCP present.
2. **Official-client control, with MCP.** Same setup plus a stdio MCP server
   exposing `read_file`/`list_dir`: `globToolCall` ×2 → `getMcpToolsToolCall`
   → `mcpToolCall`. Native still first; MCP still second.
3. **No capability exists to send.** Zero hits for `capabilit`,
   `execution_mode`, `is_remote`, `headless` across all 492 messages of
   `agent.proto`; `mcp_file_system_options.enabled` set to both `false` and
   `true` left the rate at 4/5; ACP `fs=false, terminal=false` equals
   `fs=true, terminal=true`.
4. **Not client-side-steerable.** An explicit "native tools do not exist"
   system prompt gave 5/5 native-first, and `custom_system_prompt` is rejected
   upstream with `unknown option '--system-prompt'`.
5. **Not ours, not new.** Identical 5/5 on baseline `4823235a` and HEAD
   `5ecb91a3`; identical across three model families; reject string unchanged
   since `19c52bcb`.

---

## Non-causes (each actively disproven, not assumed)

- **Regression from the Grok cloak/canonical model-ID patch** — baseline and
  HEAD produce identical traces.
- **Max Context / context parameters** — not touched in any arm; every arm used
  identical model parameters, and the behaviour is constant across them.
- **Model cloaking / canonical ID decoding** — the model ID resolved correctly
  in every run (`cursor-grok-4.6-xhigh-fast` reached upstream verbatim); runs on
  non-cloaked IDs (`composer-2.5`, `claude-4.5-sonnet`) behave the same.
- **Credential auth / cooldown** — all runs returned HTTP 200 with
  `finish_reason=tool_calls`; no `auth_unavailable`, no cooldown, no retry.
- **MCP discovery timing** — MCP tools present in the initial run request and
  in a `RequestContext` answered 2 s before the first native attempt.
- **MCP tool naming/schema quality** — native tools are attempted even when no
  MCP tool is declared at all.
- **Parallel-tool-call bug (Issue B)** — never reproduced in this audit; kept
  out of scope as instructed.

---

## Proposed fix options

| # | Option | Class | Assessment |
| --- | --- | --- | --- |
| 1 | Send a capability that disables native exec | protocol/capability-correct | **Impossible.** No such field exists. |
| 2 | Populate `RequestContext.env` | request-context | Reorders the first pick (4/5 → 2/5 native-first) but leaves total native attempts unchanged, and would require inventing `workspace_paths`/`project_folder` values the gateway does not have. Rejected as dishonest and ineffective. |
| 3 | Set `AgentRunRequest.custom_system_prompt` | harness steering | **Blocked upstream:** `invalid_argument: unknown option '--system-prompt'`, HTTP 400 on 5/5 runs. |
| 4 | Improve the rejection payload so the model recovers in fewer attempts and stops apologising to the user | rejection semantics | The only lever that is both available and honest. Bounded, reversible, no security surface. |
| 5 | Execute native tools on the gateway host | execution policy | **Forbidden.** The gateway does not hold the caller's workspace; doing so would read gateway-local files on behalf of a remote client. Explicitly out of bounds. |

Option 4 is the only survivor, and it is a **mitigation, not a
protocol-fidelity fix**.

---

## Risk analysis

- Options 1–3 are unavailable or upstream-rejected; attempting them risks HTTP
  400s on live traffic (already observed for option 3).
- Option 4 touches one string constant and its call sites in
  `processH2SessionFrames`. Risk is that a differently-worded rejection changes
  model behaviour unpredictably; it must be measured over ≥20 runs per model
  before and after, and reverted if the native-attempt count or recovery rate
  regresses.
- Doing nothing is a legitimate outcome: correctness is unaffected today. Every
  run in this audit recovered and returned a correct `tool_calls` response.

---

## Recommendation

1. **Do not** implement a capability-signalling fix. The evidence says there is
   nothing to signal, and shipping a speculative field would be exactly the
   guess this audit was meant to prevent.
2. **Do not** synthesise `RequestContext.env` workspace metadata.
3. Record in the code, next to the reject handlers, that native-first is
   upstream-designed behaviour and that rejection is the sanctioned response —
   so this is not re-litigated.
4. Optionally pursue option 4 as a separate, measured change with its own
   before/after run counts.
5. Refresh the bundled `agent.proto` descriptor as unrelated hygiene (H7).

---

## GO / NO-GO FOR IMPLEMENTATION

**NO-GO** for a protocol/capability/workspace-metadata fix. Proven unavailable.

**Conditional GO** for option 4 only, as an explicitly labelled mitigation with
a measured before/after and a documented revert path.

---

## Evidence index

| Path | Contents |
| --- | --- |
| `evidence/cursor-tool-runtime/agent-proto-descriptor.txt` | full 492-message descriptor dump |
| `evidence/cursor-tool-runtime/runs-*.jsonl` | per-run classified event sequences |
| `evidence/cursor-tool-runtime/exp-*.jsonl` | one-variable experiment arms |
| `evidence/cursor-tool-runtime/acp-A-nofs.json`, `acp-B-fs.json` | ACP control groups |
| `evidence/cursor-tool-runtime/run_probe.py` | live probe + log classifier |
| `evidence/cursor-tool-runtime/run_experiments.sh` | experiment driver |
| `evidence/cursor-tool-runtime/acp_probe.py` | minimal ACP client |
| `evidence/cursor-tool-runtime/extract_schema.py`, `extract_bundle.py` | official-bundle primary-source extraction |
| `evidence/cursor-tool-runtime/official-relay-rpcs.jsonl` | 76 sanitized official RPCs (credentials redacted) |
| `cmd/cursorprotodump` | descriptor dumper |
| `cmd/cursorcapture/relay` | recording pass-through |

Sanitisation: no `Authorization`, OAuth token, refresh token, cookie, checksum
or client-key value was written to disk. Credential-bearing headers are stored
as `<redacted len=N>`.
