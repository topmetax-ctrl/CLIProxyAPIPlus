#!/usr/bin/env python3
"""P3.5: classify every turn deterministically via correlated usage_settled."""

from __future__ import annotations

import json
import os
import uuid
from pathlib import Path

from run_cache_hit import (
    PREFIX_A,
    TOOLS_CLAUDE,
    WireSink,
    cached_from_usage,
    claude_text_and_tools,
    claude_user_id,
    latest_json,
    merge_usage,
    openai_metadata,
    openai_text_and_tools,
    parse_sse_objects,
    post_claude,
    post_openai,
    run_logged,
)

HERE = Path(__file__).resolve().parent
OUT = HERE / "p3.5-attribution.jsonl"
REPS = int(os.environ.get("CLIPROXY_P3_REPS", "3"))
MODEL = os.environ.get("CLIPROXY_CURSOR_MODEL", "cursor-grok-4.6-xhigh-fast")
LOG = os.environ.get("CLIPROXY_LOG", "/tmp/cliproxy-p35.log")


def turn_ended_for_audit(files, audit_id: str | None) -> dict | None:
    matches = []
    for path in files:
        if not path.name.endswith("-turn_ended.json"):
            continue
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except Exception:
            continue
        if not audit_id or data.get("audit_id") == audit_id:
            matches.append(data)
    return matches[-1] if matches else None


def classify_row(settled: dict | None, http: dict, cmp: dict, tools: list) -> str:
    if settled and settled.get("classification"):
        server = settled["classification"]
        if server == "TURN_ENDED_MATCHED":
            return "TURN_ENDED_MATCHED" if cmp.get("match") else "REAL_FAILURE"
        if server in {
            "NO_TURN_ENDED_EXPECTED",
            "TERMINAL_USAGE_UNAVAILABLE_BY_PROTOCOL",
            "REAL_FAILURE",
        }:
            return server
    if http.get("status") != 200:
        return "REAL_FAILURE"
    if tools:
        return "NO_TURN_ENDED_EXPECTED"
    return "REAL_FAILURE"


def compare(varints: dict, dump: dict | None, http_usage: dict | None) -> dict:
    wire_read = varints.get("3")
    internal_read = None
    if dump and dump.get("cache_read_tokens") not in (None, ""):
        internal_read = int(dump["cache_read_tokens"])
    elif wire_read is not None:
        internal_read = int(wire_read)
    http_cached, present = cached_from_usage(http_usage)
    match = (
        wire_read is not None
        and internal_read is not None
        and present
        and http_cached is not None
        and int(wire_read) == int(internal_read) == int(http_cached)
    )
    return {
        "wire": {"cache_read_tokens": wire_read, "input_tokens": varints.get("1"), "output_tokens": varints.get("2")},
        "internal": {"cache_read_tokens": internal_read, "input_tokens": (dump or {}).get("input_tokens")},
        "http": {"cached_tokens": http_cached if present else None},
        "match": match,
    }


def record(rows: list[dict], case: str, rep: int, turn: int, protocol: str, http: dict, files, session: str, tools) -> dict:
    fingerprint = latest_json(files, "-fingerprint.json")
    settled = latest_json(files, "-usage_settled.json")
    audit = (fingerprint or {}).get("audit_id") or (settled or {}).get("audit_id")
    if settled and audit and settled.get("audit_id") not in (None, audit):
        settled = None
        for path in files:
            if path.name.endswith("-usage_settled.json"):
                data = json.loads(path.read_text(encoding="utf-8"))
                if data.get("audit_id") == audit:
                    settled = data
                    break
    dump = turn_ended_for_audit(files, audit)
    varints = {}
    if dump and isinstance(dump.get("varints"), dict):
        varints = {str(k): int(v) for k, v in dump["varints"].items()}
    usage = merge_usage(parse_sse_objects(http.get("payload") or ""))
    cmp = compare(varints, dump or settled, usage)
    row = {
        "case": case,
        "rep": rep,
        "turn": turn,
        "protocol": protocol,
        "session_id": session,
        "audit_id": audit,
        "http_status": http.get("status"),
        "classification": classify_row(settled, http, cmp, tools),
        "server_classification": (settled or {}).get("classification"),
        "reason": (settled or {}).get("reason"),
        "terminal_expected": (settled or {}).get("terminal_expected"),
        "usage_source": (settled or {}).get("usage_source"),
        "continuity": (fingerprint or {}).get("continuity"),
        **cmp,
    }
    rows.append(row)
    OUT.write_text("".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")
    print(json.dumps(row, separators=(",", ":")))
    return row


def main() -> int:
    sink = WireSink(HERE / "wire-p35")
    rows: list[dict] = []

    for rep in range(1, REPS + 1):
        session = f"p35-a-{rep}-{uuid.uuid4().hex[:8]}"
        messages = []
        for turn in range(1, 3):
            messages.append({"role": "user", "content": f"Reply with exactly the token PONG-{turn} and nothing else."})
            body = {
                "model": MODEL,
                "max_tokens": 64,
                "stream": True,
                "system": PREFIX_A,
                "metadata": {"user_id": claude_user_id(session)},
                "messages": list(messages),
            }
            http, _, files = run_logged(LOG, sink, lambda b=body: post_claude(b))
            objs = parse_sse_objects(http.get("payload") or "")
            text, tools, _ = claude_text_and_tools(objs)
            record(rows, "CUR-A", rep, turn, "claude-messages", http, files, session, tools)
            messages.append({"role": "assistant", "content": text or f"PONG-{turn}"})

        session = f"p35-b-{rep}-{uuid.uuid4().hex[:8]}"
        messages = [{"role": "system", "content": PREFIX_A}]
        for turn in range(1, 3):
            messages.append({"role": "user", "content": f"Reply with exactly the token PONG-{turn} and nothing else."})
            body = {
                "model": MODEL,
                "stream": True,
                "max_tokens": 64,
                "messages": list(messages),
                "metadata": openai_metadata(session),
            }
            http, _, files = run_logged(LOG, sink, lambda b=body: post_openai(b))
            objs = parse_sse_objects(http.get("payload") or "")
            text, tools, _ = openai_text_and_tools(objs)
            record(rows, "CUR-B", rep, turn, "openai-chat", http, files, session, tools)
            messages.append({"role": "assistant", "content": text or f"PONG-{turn}"})

        session = f"p35-c-{rep}-{uuid.uuid4().hex[:8]}"
        messages = [{"role": "user", "content": "Call echo_mock exactly once with text=cache-hit-probe. Do not answer until you have the tool result."}]
        body = {
            "model": MODEL,
            "max_tokens": 256,
            "stream": True,
            "system": PREFIX_A,
            "metadata": {"user_id": claude_user_id(session)},
            "tools": TOOLS_CLAUDE,
            "messages": messages,
        }
        http, _, files = run_logged(LOG, sink, lambda: post_claude(body))
        objs = parse_sse_objects(http.get("payload") or "")
        text, tools, _ = claude_text_and_tools(objs)
        record(rows, "CUR-C", rep, 1, "claude-messages", http, files, session, tools)
        if http.get("status") == 200 and tools:
            tool = tools[0]
            messages.append({"role": "assistant", "content": [{"type": "tool_use", "id": tool["id"], "name": tool["name"], "input": tool.get("input") or {}}]})
            messages.append({"role": "user", "content": [{"type": "tool_result", "tool_use_id": tool["id"], "content": "echo:cache-hit-probe"}]})
            body2 = {
                "model": MODEL,
                "max_tokens": 64,
                "stream": True,
                "system": PREFIX_A,
                "metadata": {"user_id": claude_user_id(session)},
                "tools": TOOLS_CLAUDE,
                "messages": messages,
            }
            http2, _, files2 = run_logged(LOG, sink, lambda: post_claude(body2))
            record(rows, "CUR-C", rep, 2, "claude-messages", http2, files2, session, [])

    counts = {}
    for row in rows:
        counts[row["classification"]] = counts.get(row["classification"], 0) + 1
    unknown = sum(1 for r in rows if r["classification"] not in {
        "TURN_ENDED_MATCHED",
        "NO_TURN_ENDED_EXPECTED",
        "TERMINAL_USAGE_UNAVAILABLE_BY_PROTOCOL",
        "REAL_FAILURE",
    })
    print(json.dumps({"rows": len(rows), "counts": counts, "unknown": unknown}, separators=(",", ":")))
    return 0 if unknown == 0 and counts.get("REAL_FAILURE", 0) == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
