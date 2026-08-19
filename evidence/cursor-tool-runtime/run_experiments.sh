#!/usr/bin/env bash
# Audit-only driver: restarts the instrumented build once per experiment and
# measures the native-tool-first rate with everything else held constant.
set -uo pipefail

cd "$(dirname "$0")/../.." || exit 1
BIN=/tmp/cliproxy-exp
CFG=evidence/cursor-tool-runtime/config-audit-8403.yaml
LOG=/tmp/audit-exp.log
PORT=8403
RUNS=${RUNS:-5}
MODEL=${MODEL:-cursor-grok-4.6-xhigh-fast}

run_exp() {
  local label="$1"; shift
  echo "############ EXPERIMENT: $label ############"
  pkill -f 'cliproxy-exp --config' 2>/dev/null
  sleep 2
  rm -f "$LOG"
  env "$@" "$BIN" --config "$CFG" --no-browser >"$LOG" 2>&1 &
  local pid=$!
  for _ in $(seq 1 30); do
    sleep 1
    if curl -s -m 3 -o /dev/null "http://127.0.0.1:$PORT/v1/models" \
        -H 'Authorization: Bearer audit-local-key-1'; then break; fi
  done
  python3 evidence/cursor-tool-runtime/run_probe.py \
    --base-url "http://127.0.0.1:$PORT" --model "$MODEL" --runs "$RUNS" \
    --log-file "$LOG" --label "$label" \
    --out "evidence/cursor-tool-runtime/exp-${label}.jsonl" 2>&1 | tail -3
  echo "--- audit markers ---"
  rg -o 'cursor-audit: [^"]*' "$LOG" | sort | uniq -c | head -8
  kill $pid 2>/dev/null
  sleep 1
}

run_exp control
run_exp mcpfs-off CURSOR_EXP_MCPFS=0
run_exp mcpfs-on CURSOR_EXP_MCPFS=1
run_exp rcenv CURSOR_EXP_ENV=1 CURSOR_EXP_ENV_OS="remote-gateway" CURSOR_EXP_ENV_SHELL="none" CURSOR_EXP_ENV_PROJECT=""
run_exp customsysprompt CURSOR_EXP_SYSPROMPT="You are served through a remote API gateway. The built-in Cursor tools (read_file, write, shell, grep, ls, glob) are NOT available: every call is rejected. Use only the client-provided MCP tools."
