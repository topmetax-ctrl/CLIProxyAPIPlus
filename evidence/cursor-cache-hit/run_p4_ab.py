#!/usr/bin/env python3
"""P4 same-semantic warm checkpoint vs forced-cold flatten A/B."""

from __future__ import annotations

import hashlib
import json
import os
import statistics
import uuid
from pathlib import Path

from run_cache_hit import (
    PREFIX_A,
    PREFIX_A_SHA,
    WireSink,
    cached_from_usage,
    latest_json,
    merge_usage,
    openai_metadata,
    openai_text_and_tools,
    parse_sse_objects,
    post_openai,
    run_logged,
)

HERE = Path(__file__).resolve().parent
FIX = HERE / "cold-flatten" / "fixtures"
OUT_DIR = HERE / "cold-flatten"
REPS = int(os.environ.get("CLIPROXY_P4_REPS", "5"))
MODEL = os.environ.get("CLIPROXY_CURSOR_MODEL", "cursor-grok-4.6-xhigh-fast")
LOG = os.environ.get("CLIPROXY_LOG", "/tmp/cliproxy-p4.log")
TOOLS = json.loads((FIX / "tools.json").read_text())["openai"]
CONV = json.loads((FIX / "conversation.json").read_text())


def write_system_fixture() -> None:
    path = FIX / "system.txt"
    path.write_text(PREFIX_A, encoding="utf-8")
    (FIX / "system.sha256").write_text(PREFIX_A_SHA + "\n", encoding="utf-8")


def sha_file(path: Path) -> str:
    return hashlib.sha256(path.read_bytes()).hexdigest()


def post_openai_mode(body: dict, mode: str) -> dict:
    return post_openai(body, extra_headers={"X-Cursor-Continuation-Mode": mode})


def row_from(http: dict, files, mode: str, arm: str, rep: int, turn: int, session: str) -> dict:
    fingerprint = latest_json(files, "-fingerprint.json")
    settled = latest_json(files, "-usage_settled.json")
    usage = merge_usage(parse_sse_objects(http.get("payload") or ""))
    cached, present = cached_from_usage(usage)
    cache_read = None
    if settled and settled.get("cache_read_tokens") is not None:
        cache_read = int(settled["cache_read_tokens"])
    elif present:
        cache_read = cached
    return {
        "arm": arm,
        "mode": mode,
        "rep": rep,
        "turn": turn,
        "session_id": session,
        "audit_id": (fingerprint or {}).get("audit_id") or (settled or {}).get("audit_id"),
        "http_status": http.get("status"),
        "ttft_ms": http.get("ttft_ms"),
        "wall_ms": http.get("wall_ms"),
        "continuity": (fingerprint or {}).get("continuity"),
        "has_raw_checkpoint": (fingerprint or {}).get("has_raw_checkpoint"),
        "system_sha256": (fingerprint or {}).get("system_sha256"),
        "tools_sha256": (fingerprint or {}).get("tools_sha256"),
        "user_text_sha256": (fingerprint or {}).get("user_text_sha256"),
        "history_prefix_sha256": (fingerprint or {}).get("history_prefix_sha256"),
        "checkpoint_sha256": (fingerprint or {}).get("checkpoint_sha256"),
        "user_text_preview": (fingerprint or {}).get("user_text_preview"),
        "classification": (settled or {}).get("classification"),
        "usage_source": (settled or {}).get("usage_source"),
        "input_tokens": (settled or {}).get("input_tokens"),
        "output_tokens": (settled or {}).get("output_tokens"),
        "cache_read_tokens": cache_read,
        "cache_write_tokens": (settled or {}).get("cache_write_tokens"),
        "reasoning_tokens": (settled or {}).get("reasoning_tokens"),
    }


def median(values: list[int]) -> float | None:
    if not values:
        return None
    return statistics.median(values)


def main() -> int:
    write_system_fixture()
    sink = WireSink(OUT_DIR / "wire")
    rows: list[dict] = []
    warm_canon = None
    cold_canon = None

    for rep in range(1, REPS + 1):
        for arm, mode in (("A", "warm"), ("B", "cold")):
            session = f"p4-{arm.lower()}-{rep}-{uuid.uuid4().hex[:8]}"
            messages = [
                {"role": "system", "content": PREFIX_A},
                {"role": "user", "content": CONV["turn1_user"]},
            ]
            body1 = {
                "model": MODEL,
                "stream": True,
                "max_tokens": 64,
                "tools": TOOLS,
                "messages": messages,
                "metadata": openai_metadata(session),
            }
            http1, _, files1 = run_logged(LOG, sink, lambda b=body1: post_openai_mode(b, "auto"))
            objs = parse_sse_objects(http1.get("payload") or "")
            text, _, _ = openai_text_and_tools(objs)
            rows.append(row_from(http1, files1, "auto", arm, rep, 1, session))
            messages.append({"role": "assistant", "content": text or CONV["assistant_placeholder"]})
            messages.append({"role": "user", "content": CONV["turn2_user"]})
            body2 = {
                "model": MODEL,
                "stream": True,
                "max_tokens": 64,
                "tools": TOOLS,
                "messages": messages,
                "metadata": openai_metadata(session),
            }
            http2, _, files2 = run_logged(LOG, sink, lambda b=body2, m=mode: post_openai_mode(b, m))
            row = row_from(http2, files2, mode, arm, rep, 2, session)
            rows.append(row)
            if row.get("turn") == 2 and warm_canon is None and arm == "A":
                warm_canon = row
            if row.get("turn") == 2 and cold_canon is None and arm == "B":
                cold_canon = row

    (OUT_DIR / "results.jsonl").write_text("".join(json.dumps(r) + "\n" for r in rows), encoding="utf-8")
    if warm_canon:
        (OUT_DIR / "warm-canonical.json").write_text(json.dumps(warm_canon, indent=2) + "\n", encoding="utf-8")
    if cold_canon:
        (OUT_DIR / "cold-canonical.json").write_text(json.dumps(cold_canon, indent=2) + "\n", encoding="utf-8")

    t2_warm = [r["cache_read_tokens"] for r in rows if r["arm"] == "A" and r["turn"] == 2 and r["cache_read_tokens"] is not None]
    t2_cold = [r["cache_read_tokens"] for r in rows if r["arm"] == "B" and r["turn"] == 2 and r["cache_read_tokens"] is not None]
    summary = {
        "same_semantic_conversation": True,
        "only_continuation_path_differs": True,
        "system_sha256": PREFIX_A_SHA,
        "tools_sha256": sha_file(FIX / "tools.json"),
        "warm_reps": len(t2_warm),
        "cold_reps": len(t2_cold),
        "warm_median_cache_read": median(t2_warm),
        "cold_median_cache_read": median(t2_cold),
        "warm_min": min(t2_warm) if t2_warm else None,
        "warm_max": max(t2_warm) if t2_warm else None,
        "cold_min": min(t2_cold) if t2_cold else None,
        "cold_max": max(t2_cold) if t2_cold else None,
    }
    if summary["warm_median_cache_read"] and summary["cold_median_cache_read"]:
        summary["difference"] = summary["warm_median_cache_read"] - summary["cold_median_cache_read"]
        summary["retention"] = summary["cold_median_cache_read"] / summary["warm_median_cache_read"]
    (OUT_DIR / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary, indent=2))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
