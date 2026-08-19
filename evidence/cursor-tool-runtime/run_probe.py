#!/usr/bin/env python3
"""Live probe for the Cursor native-tool -> reject -> MCP fallback audit.

Sends one OpenAI-compatible chat request per run through a CLIProxy instance and
classifies the first tool decision the upstream Cursor agent makes by slicing the
proxy's debug log for that run. Nothing here mutates proxy behaviour; it only
observes.
"""

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request

# cursor proto ServerMessageType iota order (internal/auth/cursor/proto/decode.go).
MSG_TYPES = {
    0: "Unknown", 1: "TextDelta", 2: "ThinkingDelta", 3: "ThinkingCompleted",
    4: "KvGetBlob", 5: "KvSetBlob", 6: "ExecRequestCtx", 7: "ExecMcpArgs",
    8: "ExecShellArgs", 9: "ExecReadArgs", 10: "ExecWriteArgs", 11: "ExecDeleteArgs",
    12: "ExecLsArgs", 13: "ExecGrepArgs", 14: "ExecFetchArgs", 15: "ExecDiagnostics",
    16: "ExecShellStream", 17: "ExecBgShellSpawn", 18: "ExecWriteShellStdin",
    19: "ExecOther", 20: "TurnEnded", 21: "Heartbeat", 22: "TokenDelta",
    23: "Checkpoint",
}
NATIVE = {"ExecShellArgs", "ExecReadArgs", "ExecWriteArgs", "ExecDeleteArgs",
          "ExecLsArgs", "ExecGrepArgs", "ExecFetchArgs", "ExecShellStream",
          "ExecBgShellSpawn", "ExecWriteShellStdin"}

PROMPT = ("Read the package metadata from the current project and report the "
          "package/project name. Use the available tools rather than guessing.")

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "read_file",
            "description": "Read the contents of a file from the user's workspace.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Path to the file to read."}
                },
                "required": ["path"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "list_dir",
            "description": "List the entries of a directory in the user's workspace.",
            "parameters": {
                "type": "object",
                "properties": {
                    "path": {"type": "string", "description": "Directory path to list."}
                },
                "required": ["path"],
            },
        },
    },
]


def count_log_lines(path):
    if not path or not os.path.exists(path):
        return 0
    with open(path, "rb") as f:
        return sum(1 for _ in f)


def read_log_slice(path, start_line):
    if not path or not os.path.exists(path):
        return []
    with open(path, "r", errors="replace") as f:
        return f.readlines()[start_line:]


def classify(lines):
    """Extract the ordered upstream server-message sequence for one run."""
    seq = []
    for ln in lines:
        idx = ln.find("cursor: decoded server message type=")
        if idx == -1:
            continue
        tail = ln[idx + len("cursor: decoded server message type="):].strip()
        num = ""
        for ch in tail:
            if ch.isdigit():
                num += ch
            else:
                break
        if not num:
            continue
        name = MSG_TYPES.get(int(num), "T" + num)
        if name in ("Heartbeat", "TokenDelta", "TextDelta", "ThinkingDelta"):
            continue
        seq.append(name)
    return seq


def first_tool_decision(seq):
    for name in seq:
        if name in NATIVE:
            return "NATIVE:" + name
        if name == "ExecMcpArgs":
            return "MCP"
    return "NONE"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--base-url", required=True)
    ap.add_argument("--api-key", default="audit-local-key-1")
    ap.add_argument("--model", required=True)
    ap.add_argument("--runs", type=int, default=5)
    ap.add_argument("--log-file", default="")
    ap.add_argument("--label", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--no-tools", action="store_true")
    ap.add_argument("--prompt", default=PROMPT)
    ap.add_argument("--system", default="")
    ap.add_argument("--sleep", type=float, default=4.0)
    args = ap.parse_args()

    results = []
    for i in range(1, args.runs + 1):
        start = count_log_lines(args.log_file)
        msgs = []
        if args.system:
            msgs.append({"role": "system", "content": args.system})
        msgs.append({"role": "user", "content": args.prompt})
        body = {"model": args.model, "stream": False, "messages": msgs}
        if not args.no_tools:
            body["tools"] = TOOLS
        req = urllib.request.Request(
            args.base_url.rstrip("/") + "/v1/chat/completions",
            data=json.dumps(body).encode(),
            headers={"Content-Type": "application/json",
                     "Authorization": "Bearer " + args.api_key},
        )
        t0 = time.time()
        status, payload, err = 0, "", ""
        try:
            with urllib.request.urlopen(req, timeout=180) as resp:
                status = resp.status
                payload = resp.read().decode(errors="replace")
        except urllib.error.HTTPError as e:
            status = e.code
            payload = e.read().decode(errors="replace")
        except Exception as e:  # network/timeout
            err = repr(e)
        dt = round(time.time() - t0, 2)

        time.sleep(1.0)  # let the proxy flush its debug log
        seq = classify(read_log_slice(args.log_file, start))
        decision = first_tool_decision(seq)

        tool_calls = []
        finish = ""
        try:
            d = json.loads(payload)
            ch = (d.get("choices") or [{}])[0]
            finish = ch.get("finish_reason", "")
            for tc in (ch.get("message", {}) or {}).get("tool_calls", []) or []:
                tool_calls.append(tc.get("function", {}).get("name"))
        except Exception:
            pass

        rec = {
            "label": args.label, "run": i, "model": args.model,
            "http_status": status, "elapsed_s": dt, "error": err,
            "first_tool_decision": decision, "event_sequence": seq,
            "native_events": [s for s in seq if s in NATIVE],
            "mcp_events": seq.count("ExecMcpArgs"),
            "request_ctx_events": seq.count("ExecRequestCtx"),
            "client_tool_calls": tool_calls, "finish_reason": finish,
            "response_excerpt": payload[:400],
        }
        results.append(rec)
        print(f"[{args.label}] run {i}/{args.runs} status={status} "
              f"decision={decision} seq={seq[:8]} calls={tool_calls}", flush=True)
        if i < args.runs:
            time.sleep(args.sleep)

    with open(args.out, "w") as f:
        for r in results:
            f.write(json.dumps(r) + "\n")

    native = sum(1 for r in results if r["first_tool_decision"].startswith("NATIVE"))
    mcp = sum(1 for r in results if r["first_tool_decision"] == "MCP")
    none = sum(1 for r in results if r["first_tool_decision"] == "NONE")
    print(f"\n== {args.label} summary: runs={len(results)} native_first={native} "
          f"mcp_first={mcp} no_tool={none} ==", flush=True)
    return 0


if __name__ == "__main__":
    sys.exit(main())
