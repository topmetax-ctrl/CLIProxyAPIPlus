#!/usr/bin/env python3
"""Live tool-calling test harness against the running CLIProxyAPIPlus proxy.

Client-side only executes mock tools. Never lets the model self-execute.
"""
import json
import sys
import time
import urllib.request

BASE = "http://127.0.0.1:8317/v1/chat/completions"
KEY = "audit-local-key-1"
MODEL = sys.argv[1] if len(sys.argv) > 1 else "claude-4.5-sonnet"

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "calculator_mock",
            "description": "Evaluate a simple integer arithmetic expression like '10+20'.",
            "parameters": {
                "type": "object",
                "properties": {"expr": {"type": "string", "description": "e.g. 10+20"}},
                "required": ["expr"],
            },
        },
    },
    {
        "type": "function",
        "function": {
            "name": "get_weather_mock",
            "description": "Get mock weather for a city.",
            "parameters": {
                "type": "object",
                "properties": {"city": {"type": "string"}, "units": {"type": "string"}},
                "required": ["city"],
            },
        },
    },
]


def post(body, stream=False, timeout=90):
    req = urllib.request.Request(
        BASE,
        data=json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"},
        method="POST",
    )
    t0 = time.time()
    resp = urllib.request.urlopen(req, timeout=timeout)
    if stream:
        chunks = []
        for raw in resp:
            line = raw.decode("utf-8", "replace").strip()
            if line.startswith("data:"):
                chunks.append(line[5:].strip())
        return chunks, time.time() - t0
    return json.loads(resp.read().decode()), time.time() - t0


def run_mock(name, args):
    if name == "calculator_mock":
        try:
            expr = json.loads(args).get("expr", "")
            allowed = set("0123456789+-*/() ")
            if set(expr) <= allowed and expr.strip():
                return str(eval(expr))  # noqa: S307 - restricted charset
        except Exception:
            pass
        return "ERROR"
    if name == "get_weather_mock":
        city = json.loads(args).get("city", "?")
        return f"{city}: 25C sunny (mock)"
    return "UNKNOWN_TOOL"


def tc_single():
    body = {
        "model": MODEL,
        "messages": [
            {"role": "user", "content": "Use the calculator_mock tool to compute 21+21. You must call the tool."}
        ],
        "tools": TOOLS,
        "tool_choice": "auto",
        "stream": False,
    }
    data, dt = post(body)
    msg = data["choices"][0]["message"]
    fr = data["choices"][0]["finish_reason"]
    tcs = msg.get("tool_calls") or []
    print(f"[TOOL-001] finish_reason={fr} tool_calls={len(tcs)} latency={dt:.2f}s")
    for tc in tcs:
        print(f"   id={tc['id'][:24]}.. name={tc['function']['name']} args={tc['function']['arguments']}")
    ok = fr == "tool_calls" and len(tcs) == 1 and tcs[0]["function"]["name"] == "calculator_mock"
    return ok, data


def tc_continuation():
    ok, first = tc_single()
    if not ok:
        print("[TOOL-002] SKIP (no single tool call)")
        return False
    tc = first["choices"][0]["message"]["tool_calls"][0]
    result = run_mock(tc["function"]["name"], tc["function"]["arguments"])
    body = {
        "model": MODEL,
        "messages": [
            {"role": "user", "content": "Use the calculator_mock tool to compute 21+21. You must call the tool."},
            {"role": "assistant", "content": None, "tool_calls": [tc]},
            {"role": "tool", "tool_call_id": tc["id"], "content": result},
        ],
        "tools": TOOLS,
        "stream": False,
    }
    data, dt = post(body)
    content = data["choices"][0]["message"].get("content", "")
    fr = data["choices"][0]["finish_reason"]
    print(f"[TOOL-002] continuation finish_reason={fr} latency={dt:.2f}s content={content[:120]!r}")
    return "42" in content


def tc_parallel(runs=10):
    prompt = (
        "You must call the calculator_mock tool three times IN PARALLEL in a single turn, "
        "one for each: 10+20, 30+40, 50+60. Emit all three tool calls at once."
    )
    counts = []
    for i in range(runs):
        body = {
            "model": MODEL,
            "messages": [{"role": "user", "content": prompt}],
            "tools": TOOLS,
            "tool_choice": "auto",
            "stream": False,
        }
        try:
            data, dt = post(body)
        except Exception as e:
            print(f"[TOOL-004] run {i+1}: ERROR {e}")
            counts.append(-1)
            continue
        tcs = data["choices"][0]["message"].get("tool_calls") or []
        counts.append(len(tcs))
        print(f"[TOOL-004] run {i+1}/{runs}: tool_calls_in_turn={len(tcs)} latency={dt:.2f}s")
    good = [c for c in counts if c >= 0]
    maxcalls = max(good) if good else 0
    print(f"[TOOL-004] SUMMARY runs={runs} counts={counts} max_parallel={maxcalls}")
    return maxcalls


def tc_parallel_stream(runs=5):
    prompt = (
        "You must call the calculator_mock tool three times IN PARALLEL in a single turn, "
        "one for each: 10+20, 30+40, 50+60. Emit all three tool calls at once."
    )
    maxidx_all = []
    for i in range(runs):
        body = {
            "model": MODEL,
            "messages": [{"role": "user", "content": prompt}],
            "tools": TOOLS,
            "tool_choice": "auto",
            "stream": True,
        }
        chunks, dt = post(body, stream=True)
        idxs = set()
        for c in chunks:
            if c == "[DONE]":
                continue
            try:
                d = json.loads(c)
                for tc in d["choices"][0]["delta"].get("tool_calls") or []:
                    idxs.add(tc.get("index"))
            except Exception:
                pass
        maxidx_all.append(len(idxs))
        print(f"[TOOL-005] run {i+1}/{runs}: distinct_tool_indexes={sorted(idxs)} latency={dt:.2f}s")
    print(f"[TOOL-005] SUMMARY distinct_index_counts={maxidx_all}")
    return max(maxidx_all) if maxidx_all else 0


if __name__ == "__main__":
    which = sys.argv[2] if len(sys.argv) > 2 else "all"
    if which in ("single", "all"):
        tc_single()
    if which in ("cont", "all"):
        tc_continuation()
    if which in ("parallel", "all"):
        tc_parallel(10)
    if which in ("pstream", "all"):
        tc_parallel_stream(5)
