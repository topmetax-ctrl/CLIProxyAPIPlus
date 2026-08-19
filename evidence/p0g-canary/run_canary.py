#!/usr/bin/env python3
"""P0-G live canary against CLIProxy :8317 (Claude /v1/messages).

Does not dump prompts into reports beyond a short excerpt. Correlation comes
from proxy debug logs: generation_id / source_request_id / consumer_request_id.
"""

from __future__ import annotations

import argparse
import json
import os
import sys
import time
import urllib.error
import urllib.request
import uuid
from typing import Any

BASE_URL = "http://127.0.0.1:8317"
API_KEY = "audit-local-key-1"
MODEL = "cursor-grok-4.6-xhigh-fast"
LOG_FILE = "/tmp/cliproxy-p0g.log"

TOOLS = [
    {
        "name": "read_file",
        "description": "Read a file from the workspace. Use this instead of guessing.",
        "input_schema": {
            "type": "object",
            "properties": {"path": {"type": "string"}},
            "required": ["path"],
        },
    },
    {
        "name": "list_dir",
        "description": "List a directory. Use this instead of guessing.",
        "input_schema": {
            "type": "object",
            "properties": {"path": {"type": "string"}},
            "required": ["path"],
        },
    },
]

SESSION_EVENTS = (
    "cursor session park",
    "cursor session restore",
    "cursor tool result match",
    "cursor session replace",
    "replaced while previous",
    "do not intersect parked",
    "tool results do not match any pending",
    "MIXED_TOOL_RESULT",
    "TOOL_RESULT_ALREADY_CONSUMED",
    "TOOL_RESULT_NOT_FOUND",
    "auth_unavailable",
)


def user_id(session_id: str, parent: str = "") -> str:
    payload = {"session_id": session_id, "device_id": "p0g-canary"}
    if parent:
        payload["parent_session_id"] = parent
    return json.dumps(payload, separators=(",", ":"))


def log_size(path: str) -> int:
    try:
        return os.path.getsize(path)
    except FileNotFoundError:
        return 0


def read_log_from(path: str, start: int) -> str:
    with open(path, "rb") as f:
        f.seek(start)
        return f.read().decode("utf-8", "replace")


def event_lines(blob: str) -> list[str]:
    out = []
    for line in blob.splitlines():
        if any(n in line for n in SESSION_EVENTS) or "generation_id=" in line:
            out.append(line)
    return out


def parse_field(line: str, key: str) -> str | None:
    token = key + "="
    idx = line.find(token)
    if idx < 0:
        return None
    rest = line[idx + len(token) :]
    if rest.startswith('"'):
        end = rest.find('"', 1)
        return rest[1:end] if end > 0 else rest[1:]
    return rest.split(" ", 1)[0]


def post_messages(body: dict[str, Any], timeout: int = 180) -> tuple[int, str]:
    data = json.dumps(body).encode()
    req = urllib.request.Request(
        BASE_URL.rstrip("/") + "/v1/messages",
        data=data,
        headers={
            "Content-Type": "application/json",
            "Authorization": "Bearer " + API_KEY,
            "x-api-key": API_KEY,
            "anthropic-version": "2023-06-01",
        },
        method="POST",
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode("utf-8", "replace")


def parse_sse(payload: str) -> dict[str, Any]:
    tools: list[dict[str, Any]] = []
    text: list[str] = []
    stop = ""
    current: dict[str, Any] | None = None
    json_buf = ""
    for raw in payload.splitlines():
        if not raw.startswith("data:"):
            continue
        data = raw[5:].strip()
        if not data or data == "[DONE]":
            continue
        try:
            ev = json.loads(data)
        except json.JSONDecodeError:
            continue
        typ = ev.get("type")
        if typ == "content_block_start":
            block = ev.get("content_block") or {}
            if block.get("type") == "tool_use":
                current = {
                    "id": block.get("id") or "",
                    "name": block.get("name") or "",
                    "input": block.get("input") or {},
                }
                json_buf = ""
            elif block.get("type") == "text" and block.get("text"):
                text.append(block["text"])
        elif typ == "content_block_delta":
            delta = ev.get("delta") or {}
            if delta.get("type") == "text_delta" and delta.get("text"):
                text.append(delta["text"])
            elif delta.get("type") == "input_json_delta":
                json_buf += delta.get("partial_json") or ""
        elif typ == "content_block_stop":
            if current is not None:
                if json_buf:
                    try:
                        current["input"] = json.loads(json_buf)
                    except json.JSONDecodeError:
                        current["input_raw"] = json_buf
                tools.append(current)
                current = None
                json_buf = ""
        elif typ == "message_delta":
            stop = (ev.get("delta") or {}).get("stop_reason") or stop
        elif typ == "error":
            return {
                "tools": tools,
                "text": "".join(text),
                "stop": "error",
                "error": ev.get("error") or ev,
            }
    if current is not None:
        tools.append(current)
    return {"tools": tools, "text": "".join(text), "stop": stop, "error": None}


def fake_result(tool: dict[str, Any]) -> str:
    name = tool.get("name") or ""
    path = (tool.get("input") or {}).get("path", "")
    if name == "list_dir":
        return "README.md\npackage.json\n"
    return f"canary-ok path={path or '/etc/hosts'}\n"


def claude_body(session_id: str, messages: list[Any], max_tokens: int = 512, parent: str = "") -> dict[str, Any]:
    return {
        "model": MODEL,
        "max_tokens": max_tokens,
        "stream": True,
        "metadata": {"user_id": user_id(session_id, parent)},
        "tools": TOOLS,
        "messages": messages,
    }


def run_turn(session_id: str, messages: list[Any], label: str, parent: str = "") -> dict[str, Any]:
    start = log_size(LOG_FILE)
    t0 = time.time()
    status, payload = post_messages(claude_body(session_id, messages, parent=parent))
    parsed = parse_sse(payload) if payload.lstrip().startswith("event:") or "data:" in payload else {
        "tools": [],
        "text": payload[:500],
        "stop": "",
        "error": payload[:500] if status >= 400 else None,
    }
    rec = {
        "label": label,
        "http_status": status,
        "elapsed_s": round(time.time() - t0, 2),
        "stop": parsed.get("stop"),
        "tool_ids": [t.get("id") for t in parsed.get("tools") or []],
        "tool_names": [t.get("name") for t in parsed.get("tools") or []],
        "error": parsed.get("error"),
        "text_excerpt": (parsed.get("text") or "")[:240],
        "log_start": start,
        "log_end": log_size(LOG_FILE),
        "events": event_lines(read_log_from(LOG_FILE, start)),
    }
    rec["parsed_tools"] = parsed.get("tools") or []
    return rec


def summarize_events(events: list[str]) -> dict[str, Any]:
    parks, restores, matches = [], [], []
    miss = mixed = dup = replace_pending = 0
    for line in events:
        ev = parse_field(line, "event") or ""
        if "cursor session park" in line or ev == "cursor_session_park":
            parks.append({
                "generation_id": parse_field(line, "generation_id"),
                "source_request_id": parse_field(line, "source_request_id"),
                "pending_count": parse_field(line, "pending_count"),
            })
        if "cursor session restore" in line or ev == "cursor_session_restore":
            restores.append({
                "generation_id": parse_field(line, "generation_id"),
                "source_request_id": parse_field(line, "source_request_id"),
                "consumer_request_id": parse_field(line, "consumer_request_id"),
            })
        if "cursor tool result match" in line or ev == "cursor_tool_result_match":
            matches.append({
                "generation_id": parse_field(line, "generation_id"),
                "intersection_count": parse_field(line, "intersection_count"),
                "source_request_id": parse_field(line, "source_request_id"),
                "consumer_request_id": parse_field(line, "consumer_request_id"),
            })
        if "do not intersect" in line or parse_field(line, "intersection_count") == "0":
            miss += 1
        if "MIXED_TOOL_RESULT" in line:
            mixed += 1
        if "TOOL_RESULT_ALREADY_CONSUMED" in line:
            dup += 1
        if "replaced while previous" in line:
            replace_pending += 1
    return {
        "parks": parks,
        "restores": restores,
        "matches": matches,
        "intersect_zero": miss,
        "mixed": mixed,
        "duplicate": dup,
        "replace_with_pending": replace_pending,
    }


def print_rec(rec: dict[str, Any]) -> None:
    print(
        f"[{rec['label']}] http={rec['http_status']} stop={rec['stop']} "
        f"tools={rec['tool_ids']} err={rec['error']!r} {rec['elapsed_s']}s",
        flush=True,
    )
    for line in rec["events"]:
        print("   LOG", line, flush=True)
    print("   SUM", json.dumps(summarize_events(rec["events"]), ensure_ascii=False), flush=True)


def c1() -> dict[str, Any]:
    sid = "p0g-c1-" + uuid.uuid4().hex[:8]
    user = "Call read_file exactly once with path /etc/hosts. Do not answer without the tool."
    first = run_turn(sid, [{"role": "user", "content": user}], "C1-park")
    print_rec(first)
    if not first["tool_ids"]:
        return {"case": "C1", "ok": False, "reason": "no tool_use on first turn", "turns": [first]}
    tools = first["parsed_tools"]
    assistant = [{"type": "tool_use", "id": t["id"], "name": t["name"], "input": t.get("input") or {}} for t in tools]
    results = [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools]
    second = run_turn(
        sid,
        [
            {"role": "user", "content": user},
            {"role": "assistant", "content": assistant},
            {"role": "user", "content": results},
        ],
        "C1-result",
    )
    print_rec(second)
    summ = summarize_events(first["events"] + second["events"])
    park_gen = (summ["parks"][0]["generation_id"] if summ["parks"] else None)
    restore_gen = (summ["restores"][0]["generation_id"] if summ["restores"] else None)
    match_ok = any((m.get("intersection_count") or "0") not in ("0", "", None) for m in summ["matches"])
    ok = (
        first["http_status"] == 200
        and second["http_status"] == 200
        and park_gen
        and restore_gen == park_gen
        and match_ok
        and summ["intersect_zero"] == 0
        and summ["replace_with_pending"] == 0
    )
    return {
        "case": "C1",
        "ok": ok,
        "session_id": sid,
        "park_generation": park_gen,
        "restore_generation": restore_gen,
        "summary": summ,
        "turns": [{k: v for k, v in t.items() if k != "parsed_tools"} for t in (first, second)],
    }


def c2() -> dict[str, Any]:
    sid = "p0g-c2-" + uuid.uuid4().hex[:8]
    user_a = "Call read_file exactly once with path /etc/hosts. Do not answer without the tool."
    user_b = "Call list_dir exactly once with path /tmp. Do not answer without the tool."
    a = run_turn(sid, [{"role": "user", "content": user_a}], "C2-A-park")
    print_rec(a)
    if not a["tool_ids"]:
        return {"case": "C2", "ok": False, "reason": "A produced no tool_use", "turns": [a]}
    b = run_turn(sid, [{"role": "user", "content": user_b}], "C2-B-park")
    print_rec(b)
    tools_a = a["parsed_tools"]
    assistant_a = [{"type": "tool_use", "id": t["id"], "name": t["name"], "input": t.get("input") or {}} for t in tools_a]
    results_a = [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools_a]
    a_res = run_turn(
        sid,
        [
            {"role": "user", "content": user_a},
            {"role": "assistant", "content": assistant_a},
            {"role": "user", "content": results_a},
        ],
        "C2-A-result",
    )
    print_rec(a_res)
    summ = summarize_events(a["events"] + b["events"] + a_res["events"])
    gen_a = summ["parks"][0]["generation_id"] if summ["parks"] else None
    gen_b = summ["parks"][1]["generation_id"] if len(summ["parks"]) > 1 else None
    restore_gen = summ["restores"][0]["generation_id"] if summ["restores"] else None
    ok = (
        a["http_status"] == 200
        and b["http_status"] == 200
        and a_res["http_status"] == 200
        and gen_a
        and gen_b
        and gen_a != gen_b
        and restore_gen == gen_a
        and summ["intersect_zero"] == 0
        and summ["replace_with_pending"] == 0
        and a_res["http_status"] != 500
    )
    out: dict[str, Any] = {
        "case": "C2",
        "ok": ok,
        "session_id": sid,
        "gen_a": gen_a,
        "gen_b": gen_b,
        "restore_generation": restore_gen,
        "summary": summ,
        "turns": [{k: v for k, v in t.items() if k != "parsed_tools"} for t in (a, b, a_res)],
    }
    if b["tool_ids"]:
        tools_b = b["parsed_tools"]
        assistant_b = [{"type": "tool_use", "id": t["id"], "name": t["name"], "input": t.get("input") or {}} for t in tools_b]
        results_b = [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools_b]
        b_res = run_turn(
            sid,
            [
                {"role": "user", "content": user_b},
                {"role": "assistant", "content": assistant_b},
                {"role": "user", "content": results_b},
            ],
            "C2-B-result",
        )
        print_rec(b_res)
        out["turns"].append({k: v for k, v in b_res.items() if k != "parsed_tools"})
        out["b_result_http"] = b_res["http_status"]
        out["ok"] = bool(out["ok"] and b_res["http_status"] == 200)
    return out


def c3() -> dict[str, Any]:
    sid = "p0g-c3-" + uuid.uuid4().hex[:8]
    user_a = "Call read_file exactly once with path /etc/hosts. Do not answer without the tool."
    user_b = "Call list_dir exactly once with path /tmp. Do not answer without the tool."
    a = run_turn(sid, [{"role": "user", "content": user_a}], "C3-A-park")
    print_rec(a)
    b = run_turn(sid, [{"role": "user", "content": user_b}], "C3-B-park")
    print_rec(b)
    if not a["tool_ids"] or not b["tool_ids"]:
        return {"case": "C3", "ok": False, "reason": "missing tool_use", "turns": [a, b]}
    def resume(label: str, user: str, rec: dict[str, Any]) -> dict[str, Any]:
        tools = rec["parsed_tools"]
        assistant = [{"type": "tool_use", "id": t["id"], "name": t["name"], "input": t.get("input") or {}} for t in tools]
        results = [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools]
        return run_turn(
            sid,
            [
                {"role": "user", "content": user},
                {"role": "assistant", "content": assistant},
                {"role": "user", "content": results},
            ],
            label,
        )
    b_res = resume("C3-B-result", user_b, b)
    print_rec(b_res)
    a_res = resume("C3-A-result", user_a, a)
    print_rec(a_res)
    summ = summarize_events(a["events"] + b["events"] + b_res["events"] + a_res["events"])
    ok = (
        all(t["http_status"] == 200 for t in (a, b, b_res, a_res))
        and len(summ["parks"]) >= 2
        and summ["intersect_zero"] == 0
        and summ["replace_with_pending"] == 0
    )
    return {
        "case": "C3",
        "ok": ok,
        "session_id": sid,
        "summary": summ,
        "turns": [{k: v for k, v in t.items() if k != "parsed_tools"} for t in (a, b, b_res, a_res)],
    }


def c4() -> dict[str, Any]:
    sid = "p0g-c4-" + uuid.uuid4().hex[:8]
    user = (
        "Call read_file three times in one turn, with paths /etc/hosts, /etc/hostname, "
        "and /etc/passwd. Do not answer without those three tool calls."
    )
    first = run_turn(sid, [{"role": "user", "content": user}], "C4-park")
    print_rec(first)
    tools = first["parsed_tools"]
    park_pending = 0
    for p in summarize_events(first["events"]).get("parks") or []:
        try:
            park_pending = max(park_pending, int(p.get("pending_count") or 0))
        except (TypeError, ValueError):
            pass
    if not tools:
        return {"case": "C4", "ok": False, "reason": "no client-visible tool_use", "turns": [first], "park_pending": park_pending}
    # The H2 wait loop needs every ID in a parked batch. If Claude only surfaced
    # a subset, do not resume A (that would hang). Park B instead to prove the
    # multi-tool generation is not evicted.
    turns = [first]
    if len(tools) >= 2 and len(tools) >= park_pending:
        assistant = [{"type": "tool_use", "id": t["id"], "name": t["name"], "input": t.get("input") or {}} for t in tools]
        p1 = run_turn(
            sid,
            [
                {"role": "user", "content": user},
                {"role": "assistant", "content": assistant},
                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools[:1]]},
            ],
            "C4-partial",
        )
        print_rec(p1)
        turns.append(p1)
        rest = tools[1:]
        p2 = run_turn(
            sid,
            [
                {"role": "user", "content": user},
                {"role": "assistant", "content": assistant},
                {"role": "user", "content": [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools]},
            ],
            "C4-rest",
        )
        print_rec(p2)
        turns.append(p2)
        rest = rest  # keep for type checkers
    user_b = "Call list_dir exactly once with path /tmp. Do not answer without the tool."
    b = run_turn(sid, [{"role": "user", "content": user_b}], "C4-B-park")
    print_rec(b)
    turns.append(b)
    summ = summarize_events(sum((t["events"] for t in turns), []))
    park_gens = [p.get("generation_id") for p in summ.get("parks") or [] if p.get("generation_id")]
    ok = (
        first["http_status"] == 200
        and b["http_status"] == 200
        and park_pending >= 2
        and len(set(park_gens)) >= 2
        and summ["intersect_zero"] == 0
        and summ["replace_with_pending"] == 0
    )
    return {
        "case": "C4",
        "ok": ok,
        "session_id": sid,
        "tool_count_visible": len(tools),
        "park_pending": park_pending,
        "summary": summ,
        "turns": [{k: v for k, v in t.items() if k != "parsed_tools"} for t in turns],
    }


def c5() -> dict[str, Any]:
    sid = "p0g-c5-" + uuid.uuid4().hex[:8]
    user = "Call read_file exactly once with path /etc/hosts. Do not answer without the tool."
    first = run_turn(sid, [{"role": "user", "content": user}], "C5-park")
    print_rec(first)
    if not first["tool_ids"]:
        return {"case": "C5", "ok": False, "reason": "no tool_use", "turns": [first]}
    tools = first["parsed_tools"]
    assistant = [{"type": "tool_use", "id": t["id"], "name": t["name"], "input": t.get("input") or {}} for t in tools]
    results = [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools]
    msgs = [
        {"role": "user", "content": user},
        {"role": "assistant", "content": assistant},
        {"role": "user", "content": results},
    ]
    consumed = run_turn(sid, msgs, "C5-consume")
    print_rec(consumed)
    retry = run_turn(sid, msgs, "C5-retry-duplicate")
    print_rec(retry)
    follow = c1_followup("C5-followup-unrelated")
    duplicate_local = retry["http_status"] in (400, 409) or (
        isinstance(retry.get("error"), str) and "ALREADY_CONSUMED" in retry["error"]
    ) or any("ALREADY_CONSUMED" in e or "already consumed" in e.lower() for e in retry["events"]) or (
        retry["http_status"] >= 400 and retry["http_status"] != 503
    )
    no_cooldown = follow["http_status"] == 200 and follow.get("stop") != "error"
    ok = consumed["http_status"] == 200 and duplicate_local and no_cooldown and follow["http_status"] != 503
    return {
        "case": "C5",
        "ok": ok,
        "session_id": sid,
        "retry_http": retry["http_status"],
        "follow_http": follow["http_status"],
        "duplicate_classified_local": duplicate_local,
        "no_cooldown": no_cooldown,
        "turns": [{k: v for k, v in t.items() if k != "parsed_tools"} for t in (first, consumed, retry, follow)],
    }


def c1_followup(label: str) -> dict[str, Any]:
    sid = "p0g-follow-" + uuid.uuid4().hex[:8]
    rec = run_turn(sid, [{"role": "user", "content": "Reply with the single word pong. Do not use tools."}], label)
    print_rec(rec)
    return rec


def c6() -> dict[str, Any]:
    main_sid = "p0g-c6-main-" + uuid.uuid4().hex[:8]
    sub_sid = "p0g-c6-sub-" + uuid.uuid4().hex[:8]
    user_main = "Call read_file exactly once with path /etc/hosts. Do not answer without the tool."
    user_sub = "Call list_dir exactly once with path /tmp. Do not answer without the tool."
    main = run_turn(main_sid, [{"role": "user", "content": user_main}], "C6-main-park")
    print_rec(main)
    sub = run_turn(sub_sid, [{"role": "user", "content": user_sub}], "C6-sub-park", parent=main_sid)
    print_rec(sub)
    if not main["tool_ids"]:
        return {"case": "C6", "ok": False, "reason": "main produced no tool_use", "turns": [main, sub]}
    tools = main["parsed_tools"]
    assistant = [{"type": "tool_use", "id": t["id"], "name": t["name"], "input": t.get("input") or {}} for t in tools]
    results = [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools]
    main_res = run_turn(
        main_sid,
        [
            {"role": "user", "content": user_main},
            {"role": "assistant", "content": assistant},
            {"role": "user", "content": results},
        ],
        "C6-main-result",
    )
    print_rec(main_res)
    summ = summarize_events(main["events"] + sub["events"] + main_res["events"])
    ok = (
        main["http_status"] == 200
        and sub["http_status"] == 200
        and main_res["http_status"] == 200
        and summ["intersect_zero"] == 0
        and summ["replace_with_pending"] == 0
    )
    return {
        "case": "C6",
        "ok": ok,
        "main_session": main_sid,
        "sub_session": sub_sid,
        "summary": summ,
        "turns": [{k: v for k, v in t.items() if k != "parsed_tools"} for t in (main, sub, main_res)],
    }


def p0e_unknown() -> dict[str, Any]:
    sid = "p0g-p0e-" + uuid.uuid4().hex[:8]
    user = "Call read_file exactly once with path /etc/hosts. Do not answer without the tool."
    parked = run_turn(sid, [{"role": "user", "content": user}], "P0E-park")
    print_rec(parked)
    if not parked["tool_ids"]:
        return {"case": "P0E", "ok": False, "reason": "no tool_use to miss against", "turns": [parked]}
    tools = parked["parsed_tools"]
    assistant = [{"type": "tool_use", "id": t["id"], "name": t["name"], "input": t.get("input") or {}} for t in tools]
    bogus = run_turn(
        sid,
        [
            {"role": "user", "content": user},
            {"role": "assistant", "content": assistant},
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": "cursor_call_p0g_unknown", "content": "nope"}]},
        ],
        "P0E-unknown-result",
    )
    print_rec(bogus)
    follow = c1_followup("P0E-followup-same-account")
    real = run_turn(
        sid,
        [
            {"role": "user", "content": user},
            {"role": "assistant", "content": assistant},
            {"role": "user", "content": [{"type": "tool_result", "tool_use_id": t["id"], "content": fake_result(t)} for t in tools]},
        ],
        "P0E-real-result",
    )
    print_rec(real)
    local = bogus["http_status"] in (400, 404, 409) or (
        bogus["http_status"] >= 400 and bogus["http_status"] != 503
    )
    ok = (
        local
        and follow["http_status"] == 200
        and follow["http_status"] != 503
        and real["http_status"] == 200
        and "auth_unavailable" not in str(follow.get("error") or "")
    )
    return {
        "case": "P0E",
        "ok": ok,
        "unknown_http": bogus["http_status"],
        "follow_http": follow["http_status"],
        "real_http": real["http_status"],
        "turns": [{k: v for k, v in t.items() if k != "parsed_tools"} for t in (parked, bogus, follow, real)],
    }


CASES = {
    "C1": c1,
    "C2": c2,
    "C3": c3,
    "C4": c4,
    "C5": c5,
    "C6": c6,
    "P0E": p0e_unknown,
}


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("cases", nargs="*", default=["C1"], help="C1 C2 C3 C4 C5 C6 P0E")
    ap.add_argument("--out", default="evidence/p0g-canary/results.jsonl")
    args = ap.parse_args()
    os.makedirs(os.path.dirname(args.out) or ".", exist_ok=True)
    results = []
    rc = 0
    for name in args.cases:
        fn = CASES.get(name.upper())
        if fn is None:
            print("unknown case", name, file=sys.stderr)
            rc = 2
            continue
        print(f"\n======== {name.upper()} ========", flush=True)
        rec = fn()
        rec["at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        results.append(rec)
        print(f"RESULT {name.upper()} ok={rec.get('ok')} {json.dumps({k: rec[k] for k in rec if k != 'turns'}, default=str)}", flush=True)
        if not rec.get("ok"):
            rc = 1
    with open(args.out, "a") as f:
        for rec in results:
            f.write(json.dumps(rec, default=str) + "\n")
    return rc


if __name__ == "__main__":
    sys.exit(main())
