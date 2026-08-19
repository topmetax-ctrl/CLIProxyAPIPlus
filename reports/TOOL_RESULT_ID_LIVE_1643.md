# Live re-audit 2026-08-19 16:43 — do not infer client from system prompt

Status: **ROOT CAUSE NOT FULLY PROVEN** (pending-session contents not logged).  
**GO FOR IMPLEMENTATION: NO**

The screenshot at 16:43:34 is **not** the 15:13 `cursorcall` sanitizer incident. Treating them as one bug was wrong.

---

## What the 16:43 screenshot actually is

Kiro-Go admin row `2362f86c-5f2e-40ea-9aa4-6fca1effbea2`:

- Client model `claude-opus-5-thinking` → target `cursor-grok-4.6-xhigh-fast`
- Provider `Cursor Cliproxy`, endpoint `claude`, stream yes
- Upstream + client status 500, ~221 ms, 0/0 tokens
- Error: `cursor: tool results do not match any pending tool call`

That error is raised **locally** in CLIProxy `resumeWithToolResults` before anything is sent to Cursor. Zero tokens and ~200 ms match that path.

---

## Actual caller (from headers / process, not prompt text)

```
Claude Code CLI 2.1.201 (cc_entrypoint=cli)
  user doanbh, cwd /Volumes/DoanBHSST9/Outsource/k_pos, SuperClaude
  POST /v1/messages  (Anthropic Messages + claude-code betas)
       ↓  Cloudflare tunnel / LAN
Docker Kiro-Go :8080  (container kiro-go-kiro-go-1)
  route b37c7901-f4d8-4166-9475-9d0b6cd0ac1b
  raw forward + model rewrite only
       ↓  User-Agent Go-http-client/1.1, Stainless headers preserved
CLIProxy :8317  (cliproxy-audit since 15:41)
       ↓
Cursor AgentService
```

Proof this is Claude Code, not OpenClaw:

| Signal | Value |
|---|---|
| System block | `x-anthropic-billing-header: cc_version=2.1.201.cc2; cc_entrypoint=cli` |
| System block | `You are Claude Code, Anthropic's official CLI for Claude.` |
| `Anthropic-Beta` | `claude-code-20250219,...` |
| `X-Stainless-Lang` | `js` |
| `X-Stainless-Package-Version` | `0.94.0` |
| Tools | `Agent`, `Bash`, `Edit`, `AskUserQuestion`, … (27 Claude Code tools) |
| Hook | `UserPromptSubmit` with `User: doanbh` |
| `metadata.user_id.session_id` | `ef43570c-01d6-4f4a-8351-63bf3f966f68` |

`"running inside OpenClaw"` is **untrusted payload text**. It is **not** present in this 16:43 body. Using it to name the 16:43 client was incorrect.

---

## Tool IDs in this request were not mutated

Last turn (assistant 181 → user 182):

| | |
|---|---|
| Tools | WebSearch, WebSearch, Bash |
| Extra user text | `continue` |
| Public IDs | `cursor_call_` + base64url, length 127 |
| `tool_use.id == tool_result.tool_use_id` | **true** (3/3) |
| Whole transcript | 107 uses, 107 results, 0 orphans, 0 `cursorcall` (no underscore) |

Decoded Cursor internal id still contains a newline (`call-…-18\nfc_…_0`). That newline is **inside** the base64 payload. The public string stays `[A-Za-z0-9_-]` so Claude sanitizers do not rewrite it.

Kiro-Go forward on this hop only rewrites `model`. It did not strip `_` or cut to 40. **Kiro-Go ID mutation for 16:43 = DISPROVEN.**

---

## Why CLIProxy still returned 500

Error is only returned when **all** of these are true (`cursor_executor.go`):

1. Source format is Claude (not OpenAI cold-continue).
2. A parked H2 session exists for `authID + deriveConversationId(...)`.
3. `session.stream != nil` and same auth.
4. **No** `parsed.ToolResults[].ToolCallId` equals **any** `session.pending[].ToolCallId`.

So CLIProxy **did** find a session for this Claude Code `session_id`, and that session's pending IDs were **disjoint** from every tool result in the 1.3 MB body (107 IDs). That is a **pending-state / wrong-batch** failure, not a round-trip string rewrite of these IDs.

Parked pending IDs themselves are **not logged**. Exact parked strings = **UNPROVEN**.

Strong remaining hypotheses (not confirmed):

- Same `session_id` had a concurrent stream (retry, second `continue`, or `Agent` subagent) that parked a **newer** tool batch; this HTTP body is a stale snapshot of the previous batch.
- A foreign session hashed onto the same conversation key (much weaker here because `metadata.user_id.session_id` is present).

Overlapping Kiro-Go 200s at 16:44:00 (49.5 s) and 16:44:01 (11.1 s) show concurrent streams exist on this proxy. That is supporting, not proof.

Same body was retried at 16:43:34, :52, 16:44:28, 16:45:04, :43, 16:46:21 — mismatch every time. `restoreSession()` puts the unmatched session back, so retries keep hitting the same parked pending set.

---

## 500 → 503 still happens (independent)

16:43:15–24: `auth_unavailable` (cooldown).  
16:43:34: mismatch 500 again after the 15 s window.  
`transient-error-cooldown-seconds: 15` still treats this local `fmt.Errorf` as a recoverable 500 and parks the only Cursor auth+model.

That classification bug is independent of who mutated IDs, and independent of whether 16:43 is a pending-state miss.

---

## Second, concurrent incident at 16:52 (do not merge)

`error-v1-messages-2026-08-19T165212-3a8fec87.log`:

- Model `cursor-grok-4.6-high-fast` (sonnet route, not opus/xhigh)
- **No** Stainless, **no** `claude-code` beta, **no** `metadata`
- 3 messages, tools `memory_get` / `memory_get` / `browser`
- IDs already `cursorcall…` length 40, shared first 32 + 8-hex tails
- System text includes `running inside OpenClaw` — **untrusted**; do not treat as process proof
- Local `openclaw` gateway `:18789` has been running since 22 Jul — **not** proof this request came from it

For 16:52: **inbound ID mutation = CONFIRMED. Component = UNPROVEN.**

---

## Corrected verdicts

| Claim | 16:43 (screenshot) | 16:52 |
|---|---|---|
| Upstream/client-side ID mutation | **DISPROVEN** (IDs intact) | **CONFIRMED** in CLIProxy inbound body |
| Exact component that mutated IDs | n/a | **UNPROVEN** |
| OpenClaw responsibility | **NOT APPLICABLE** | **UNPROVEN** (prompt text is not process identity) |
| Kiro-Go mutates IDs | **DISPROVEN** for this body | **UNPROVEN** (forward HEAD still identity-preserving) |
| CLIProxy matcher string-compare | working as written | working as written |
| FIRST MUTATION HOP | none in this incident | between CLIProxy outbound and CLIProxy inbound-next-turn |
| Failure mode | parked pending IDs ∉ this transcript | mutated IDs ∉ parked pending |
| Cooldown → 503 | **YES** | **YES** |
| GO FOR IMPLEMENTATION | **NO** | **NO** |

---

## Model-ID patch (18–19 Aug)

See `reports/MODEL_ID_SESSION_KEY_AUDIT.md`.

```
DIRECT tool-ID mutation regression: DISPROVEN
Park/restore using different model strings: DISPROVEN
Model-in-session-key alias merge: DISPROVEN
INDIRECT session-key cardinality change: DISPROVEN
Latent one-slot overwrite race: CONFIRMED in code, pre-existing
Model-ID patch as 16:43 trigger: LOW
```

Cursor park key is `authID + ":" + conversationId`. Model is not in it. `publishConversationSession(..., replace=true)` overwrites the only generation. That race does not need the model-ID patch. Do not revert `5ecb91a3`; do not change the matcher.

---

## Still required before any fix

1. Log sanitized pending vs inbound ID hashes + park generation at the matcher (no secrets).
2. Hop-capture one Claude Code tool turn (H2 outbound vs H6 inbound) — 16:43 predicts equality.
3. Hop-capture one 16:52-class turn to name the sanitizer process (PID / User-Agent before Kiro-Go).
4. Concurrent matrix B/D/E/F (parallel / retry / continue / subagent) on one `session_id`; test H (two aliases) is expected to collide on GOOD and BAD.
5. Do not fuzzy-match, do not strip `_` to compare, do not revert the model-ID decode commit.
