# Cursor Grok prompt-cache audit

Evidence for: when calling Cursor Grok through CLIProxyAPIPlus, does the
request hit provider prompt cache, and does CLIProxy drop or reduce that
hit?

## Layout

```
evidence/cursor-cache-hit/
├── README.md                 this file
├── SUMMARY.md                verdict and tables
├── protocol.md               InteractionUpdate / TurnEnded wire
├── schema.json               results.jsonl schema
├── results.jsonl             one object per turn
├── turnended-fields.jsonl    five raw TurnEnded varints per case
├── analyze_turnended.py      field correlation
├── turnended-correlation.md  analyze output
├── cursor-app-turnended-schema.txt  Cursor.app generated field names
├── fixtures/                 stable-prefix-A/B.txt
├── p3-e2e.jsonl              live wire==internal==HTTP
├── p3-e2e.md                 P3 acceptance
├── lifecycle.md              TurnEnded settlement states
├── p3.5-attribution.jsonl    deterministic classification
├── run_p3_5.py               P3.5 harness
├── run_p4_ab.py              same-semantic warm/cold A/B
├── cold-flatten/             P4 A/B (no causal degradation)
├── run_p3_e2e.py             live mapping check
├── run_cache_hit.py          live harness
├── IDENTITY.json             preflight (proxy, models, auth)
├── log_excerpts.txt          sanitized proxy log slices
├── config-probe-8321.yaml    isolated debug instance
├── request-fingerprints/     system/tools/history sha256
├── wire/                     TurnEnded hex + json dumps
└── raw/                      extra extracts
```

## How to reproduce

1. Build a debug-gated binary (no request-semantics change):

   ```bash
   go build -o /tmp/cliproxy-cache-audit ./cmd/server
   ```

2. Run it on an unused port with dump dir set:

   ```bash
   export CURSOR_WIRE_DUMP_DIR="$PWD/evidence/cursor-cache-hit/wire"
   /tmp/cliproxy-cache-audit \
     --config evidence/cursor-cache-hit/config-probe-8321.yaml
   ```

3. Run the harness:

   ```bash
   CLIPROXY_BASE_URL=http://127.0.0.1:8321 \
   CLIPROXY_PORT=8321 \
   CLIPROXY_LOG=/path/to/proxy.log \
   python3 evidence/cursor-cache-hit/run_cache_hit.py --reps 3
   ```

Instrumentation is env-gated (`CURSOR_WIRE_DUMP_DIR`) and debug logs.
It does not alter EncodeRunRequest, checkpoints, or client usage JSON.

## What counts as a hit

- Named HTTP `cached_tokens` > 0, or
- A TurnEnded field that has been **proven** to be cache-read, with
  value > 0.

Checkpoint, park/restore, HTTP/2 reuse, and latency are not hits.
A missing field is `UNOBSERVABLE`, not `MISS`.

P1 status (see `SUMMARY.md` / `protocol.md`):

- TurnEnded fields 1–5 are **AUTHORITATIVELY MAPPED** from Cursor.app
  generated `agent.v1.TurnEndedUpdate`.
- Field 3 is `cache_read_tokens`.
- Production mapping is implemented: terminal TurnEnded →
  `cursorTokenUsage` → OpenAI `cached_tokens` / Claude
  `cache_read_input_tokens`.
- Cache *behavior* (checkpoint / flatten / routing) is unchanged.
- P3.5: every row is classified; missing TurnEnded on some fresh/EOF
  turns is `TERMINAL_USAGE_UNAVAILABLE_BY_PROTOCOL`, not a harness race.
- P4 same-semantic flatten vs checkpoint: no reproducible cache-read
  drop. No flatten fix.
