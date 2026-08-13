#!/usr/bin/env python3
"""Extra live tests: sequential tool loop, multi-turn marker, concurrency isolation."""
import json
import sys
import time
import threading
import urllib.request

BASE = "http://127.0.0.1:8317/v1/chat/completions"
KEY = "audit-local-key-1"
MODEL = sys.argv[1] if len(sys.argv) > 1 else "claude-4.5-sonnet"

TOOLS = [
    {
        "type": "function",
        "function": {
            "name": "calculator_mock",
            "description": "Evaluate one simple integer arithmetic expression like '10+20'. Call once per step.",
            "parameters": {
                "type": "object",
                "properties": {"expr": {"type": "string"}},
                "required": ["expr"],
            },
        },
    }
]


def post(body, timeout=120):
    req = urllib.request.Request(
        BASE, data=json.dumps(body).encode(),
        headers={"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"}, method="POST")
    t0 = time.time()
    resp = urllib.request.urlopen(req, timeout=timeout)
    return json.loads(resp.read().decode()), time.time() - t0


def calc(args):
    try:
        expr = json.loads(args).get("expr", "")
        if set(expr) <= set("0123456789+-*/() ") and expr.strip():
            return str(eval(expr))  # noqa
    except Exception:
        pass
    return "ERROR"


def sequential_loop(max_steps=20):
    msgs = [{
        "role": "user",
        "content": (
            "Start with 0. Then repeatedly use calculator_mock to add the next integer: "
            "first 0+1, take the result and add 2, then add 3, and so on up to adding 20. "
            "Call the tool ONE step at a time and wait for each result. "
            "When you have added through 20, reply with FINAL=<sum>."
        ),
    }]
    steps = 0
    for _ in range(max_steps + 5):
        body = {"model": MODEL, "messages": msgs, "tools": TOOLS, "tool_choice": "auto", "stream": False}
        try:
            data, dt = post(body)
        except Exception as e:
            print(f"[TOOL-003] step {steps}: ERROR {e}")
            return False
        ch = data["choices"][0]
        msg = ch["message"]
        if ch["finish_reason"] == "tool_calls" and msg.get("tool_calls"):
            tc = msg["tool_calls"][0]
            res = calc(tc["function"]["arguments"])
            steps += 1
            print(f"[TOOL-003] step {steps}: {tc['function']['arguments']} -> {res} ({dt:.1f}s)")
            msgs.append({"role": "assistant", "content": msg.get("content"), "tool_calls": [tc]})
            msgs.append({"role": "tool", "tool_call_id": tc["id"], "content": res})
            time.sleep(1.2)
        else:
            print(f"[TOOL-003] DONE after {steps} tool steps. final={msg.get('content','')[:120]!r}")
            return steps >= 15
    print(f"[TOOL-003] hit cap, steps={steps}")
    return steps >= 15


def multi_turn(turns=20):
    marker = "SESSION_MARKER_7F29"
    msgs = [{"role": "user", "content": f"Remember this code: {marker}. Just acknowledge with OK."}]
    for i in range(turns - 1):
        data, _ = post({"model": MODEL, "messages": msgs, "stream": False})
        reply = data["choices"][0]["message"]["content"]
        msgs.append({"role": "assistant", "content": reply})
        if i < turns - 2:
            msgs.append({"role": "user", "content": f"Say the word number {i+2}. Keep it short."})
        else:
            msgs.append({"role": "user", "content": "What was the code I gave you at the very start? Reply with just the code."})
    data, _ = post({"model": MODEL, "messages": msgs, "stream": False})
    final = data["choices"][0]["message"]["content"]
    print(f"[MULTI-TURN] turns={turns} recalled={marker in final} final={final[:80]!r}")
    return marker in final


def isolation(n=3):
    markers = {"A": "AAA111", "B": "BBB222", "C": "CCC333"}
    results = {}

    def worker(tag, mk):
        body = {"model": MODEL, "messages": [
            {"role": "user", "content": f"My secret token is {mk}. Reply with ONLY that token, nothing else."}
        ], "stream": False}
        try:
            data, _ = post(body)
            results[tag] = data["choices"][0]["message"]["content"]
        except Exception as e:
            results[tag] = f"ERR {e}"

    threads = [threading.Thread(target=worker, args=(t, m)) for t, m in markers.items()]
    for _ in range(n):
        for t in threads:
            t.start()
        for t in threads:
            t.join()
        threads = [threading.Thread(target=worker, args=(t, m)) for t, m in markers.items()]
    ok = True
    for tag, mk in markers.items():
        got = results.get(tag, "")
        leaked = any(other in got for o, other in markers.items() if o != tag)
        contains = mk in got
        print(f"[ISO] {tag} expect={mk} got={got[:40]!r} contains={contains} leaked_other={leaked}")
        if leaked or not contains:
            ok = False
    return ok


def concurrency(level=10):
    errs = [0]
    lat = []
    lock = threading.Lock()

    def worker(i):
        body = {"model": MODEL, "messages": [{"role": "user", "content": f"Reply with the number {i}."}], "stream": False}
        try:
            data, dt = post(body)
            with lock:
                lat.append(dt)
        except Exception:
            with lock:
                errs[0] += 1

    threads = [threading.Thread(target=worker, args=(i,)) for i in range(level)]
    t0 = time.time()
    for t in threads:
        t.start()
    for t in threads:
        t.join()
    print(f"[CONC-{level}] done in {time.time()-t0:.1f}s errors={errs[0]} ok={len(lat)} avg_lat={(sum(lat)/len(lat)) if lat else 0:.2f}s")
    return errs[0] == 0


if __name__ == "__main__":
    which = sys.argv[2] if len(sys.argv) > 2 else "all"
    if which in ("seq", "all"):
        sequential_loop(20)
    if which in ("multi", "all"):
        multi_turn(20)
    if which in ("iso", "all"):
        isolation(2)
    if which in ("conc", "all"):
        concurrency(10)
