#!/usr/bin/env python3
"""Minimal ACP client for the audit control group.

Spawns `cursor-agent acp`, declares client capabilities (fs/terminal on or off),
sends one prompt and records which tool kinds the agent reports.
"""
import argparse
import json
import os
import subprocess
import sys
import threading
import time

PROMPT = ("Read the package metadata from the current project and report the "
          "package/project name. Use the available tools rather than guessing.")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--fs", action="store_true", help="declare fs read/write capability")
    ap.add_argument("--terminal", action="store_true")
    ap.add_argument("--cwd", required=True)
    ap.add_argument("--model", default="")
    ap.add_argument("--label", required=True)
    ap.add_argument("--out", required=True)
    ap.add_argument("--timeout", type=float, default=120.0)
    args = ap.parse_args()

    proc = subprocess.Popen(
        ["cursor-agent", "acp"], cwd=args.cwd,
        stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
        text=True, bufsize=1, env={**os.environ},
    )

    events = []
    lock = threading.Lock()

    def reader():
        for line in proc.stdout:
            line = line.strip()
            if not line:
                continue
            try:
                msg = json.loads(line)
            except Exception:
                continue
            with lock:
                events.append(msg)

    threading.Thread(target=reader, daemon=True).start()
    threading.Thread(
        target=lambda: [None for _ in proc.stderr], daemon=True).start()

    def send(obj):
        proc.stdin.write(json.dumps(obj) + "\n")
        proc.stdin.flush()

    def wait_id(rid, timeout):
        end = time.time() + timeout
        while time.time() < end:
            with lock:
                for m in events:
                    if m.get("id") == rid and ("result" in m or "error" in m):
                        return m
            # Answer agent->client requests so the session can proceed.
            with lock:
                pending = [m for m in events
                           if m.get("method") and m.get("id") is not None
                           and not m.get("_answered")]
            for m in pending:
                m["_answered"] = True
                meth = m["method"]
                if meth == "session/request_permission":
                    opts = (m.get("params") or {}).get("options") or []
                    pick = next((o["optionId"] for o in opts
                                 if "allow" in o.get("optionId", "").lower()), None)
                    if pick is None and opts:
                        pick = opts[0].get("optionId")
                    send({"jsonrpc": "2.0", "id": m["id"],
                          "result": {"outcome": {"outcome": "selected", "optionId": pick}}})
                else:
                    send({"jsonrpc": "2.0", "id": m["id"], "result": {}})
            time.sleep(0.15)
        return None

    caps = {"fs": {"readTextFile": args.fs, "writeTextFile": args.fs},
            "terminal": args.terminal}
    send({"jsonrpc": "2.0", "id": 1, "method": "initialize",
          "params": {"protocolVersion": 1, "clientCapabilities": caps}})
    init = wait_id(1, 30)

    send({"jsonrpc": "2.0", "id": 2, "method": "session/new",
          "params": {"cwd": args.cwd, "mcpServers": []}})
    newsess = wait_id(2, 60)
    sid = None
    if newsess and "result" in newsess:
        sid = newsess["result"].get("sessionId")
    if not sid:
        print(f"[{args.label}] session/new failed: {json.dumps(newsess)[:300]}")
        proc.kill()
        return 1

    send({"jsonrpc": "2.0", "id": 3, "method": "session/prompt",
          "params": {"sessionId": sid,
                     "prompt": [{"type": "text", "text": PROMPT}]}})
    wait_id(3, args.timeout)
    proc.kill()

    kinds = []
    for m in events:
        if m.get("method") == "session/update":
            u = ((m.get("params") or {}).get("update") or {})
            if u.get("sessionUpdate") in ("tool_call", "tool_call_update"):
                kinds.append({"kind": u.get("kind"), "title": u.get("title"),
                              "status": u.get("status")})
    with open(args.out, "w") as f:
        json.dump({"label": args.label, "fs": args.fs, "terminal": args.terminal,
                   "initialize": init, "tool_calls": kinds}, f, indent=2)
    seen = []
    for k in kinds:
        key = (k["kind"], k["title"])
        if key not in seen:
            seen.append(key)
    print(f"[{args.label}] fs={args.fs} terminal={args.terminal} "
          f"tool_calls={len(kinds)} distinct={seen[:8]}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
