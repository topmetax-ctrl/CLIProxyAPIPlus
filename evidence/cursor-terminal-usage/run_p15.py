#!/usr/bin/env python3
"""P15 Cursor terminal-usage live matrix + P16 warm cache smoke.

Does not invent cached_tokens. Classifies every logical turn.
Does not talk to LaunchAgent :8317; use CLIPROXY_BASE_URL.
"""

from __future__ import annotations

import json
import os
import sys
import time
import uuid
from pathlib import Path

HERE = Path(__file__).resolve().parent
CACHE_HIT = HERE.parent / "cursor-cache-hit"
sys.path.insert(0, str(CACHE_HIT))

from run_cache_hit import (  # noqa: E402
    PREFIX_A,
    TOOLS_CLAUDE,
    WireSink,
    cached_from_usage,
    claude_text_and_tools,
    claude_user_id,
    merge_usage,
    openai_metadata,
    openai_text_and_tools,
    parse_sse_objects,
    post_claude,
    post_openai,
    run_logged,
)

OUT = HERE / "p15-live.jsonl"
LOG = os.environ.get("CLIPROXY_LOG", "/tmp/cliproxy-p15.log")
REPS = int(os.environ.get("CLIPROXY_P15_REPS", "5"))
MODEL = os.environ.get("CLIPROXY_CURSOR_MODEL", "cursor-grok-4.6-xhigh-fast")
WIRE = Path(os.environ.get("CURSOR_WIRE_DUMP_DIR", str(HERE / "wire-p15")))


def settled_for(files, audit: str | None) -> dict | None:
    for path in reversed(files):
        if not path.name.endswith("-usage_settled.json"):
            continue
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except Exception:
            continue
        if not audit or data.get("audit_id") == audit:
            return data
    return None


def row(kind: str, rep: int, turn: int, protocol: str, http: dict, files, session: str) -> dict:
    fingerprint = None
    for path in reversed(files):
        if path.name.endswith("-fingerprint.json"):
            try:
                fingerprint = json.loads(path.read_text(encoding="utf-8"))
                break
            except Exception:
                pass
    audit = (fingerprint or {}).get("audit_id")
    settled = settled_for(files, audit)
    usage = merge_usage(parse_sse_objects(http.get("payload") or "")) or {}
    cached, cached_present = cached_from_usage(usage)
    class_ = (settled or {}).get("classification") or "UNKNOWN"
    source = (settled or {}).get("usage_source")
    te = bool((settled or {}).get("terminal_seen"))
    rec = {
        "case": kind,
        "rep": rep,
        "turn": turn,
        "protocol": protocol,
        "session_id": session,
        "audit_id": audit,
        "http_status": http.get("status"),
        "classification": class_,
        "reason": (settled or {}).get("reason"),
        "usage_source": source,
        "turn_ended_seen": te,
        "input_tokens": (settled or {}).get("input_tokens"),
        "output_tokens": (settled or {}).get("output_tokens"),
        "cache_read_tokens": (settled or {}).get("cache_read_tokens")
        if (settled or {}).get("cache_read_present") or te
        else None,
        "cache_write_tokens": (settled or {}).get("cache_write_tokens")
        if (settled or {}).get("cache_write_present")
        else None,
        "reasoning_tokens": (settled or {}).get("reasoning_tokens")
        if (settled or {}).get("reasoning_present")
        else None,
        "http_cached_tokens": cached if cached_present else None,
        "http_cached_present": cached_present,
        "connect_end_seen": (settled or {}).get("connect_end_seen"),
        "last_frames": (settled or {}).get("last_frames"),
        "error": http.get("error"),
    }
    if cached_present and not te:
        rec["invented_cache"] = True
    return rec


def write_rows(rows: list[dict]) -> None:
    OUT.write_text("".join(json.dumps(r, ensure_ascii=False) + "\n" for r in rows), encoding="utf-8")


def main() -> int:
    WIRE.mkdir(parents=True, exist_ok=True)
    sink = WireSink(WIRE)
    rows: list[dict] = []

    def go(label, fn):
        http, _lines, files = run_logged(LOG, sink, fn)
        return http, files

    # Normal Claude
    for i in range(REPS):
        session = f"p15-n-{i}-{uuid.uuid4().hex[:8]}"
        http, files = go(
            "normal",
            lambda: post_claude(
                {
                    "model": MODEL,
                    "max_tokens": 64,
                    "stream": True,
                    "system": PREFIX_A,
                    "messages": [{"role": "user", "content": "Reply with exactly the token PONG-1 and nothing else."}],
                    "metadata": {"user_id": claude_user_id(session)},
                }
            ),
        )
        rows.append(row("normal-claude", i + 1, 1, "claude-messages", http, files, session))
        write_rows(rows)
        if http.get("status") not in (200, None) and http.get("status") in (401, 403, 429):
            print("quota/auth stop", http.get("status"))
            break

    # OpenAI
    for i in range(REPS):
        session = f"p15-o-{i}-{uuid.uuid4().hex[:8]}"
        http, files = go(
            "openai",
            lambda: post_openai(
                {
                    "model": MODEL,
                    "stream": True,
                    "messages": [
                        {"role": "system", "content": PREFIX_A},
                        {"role": "user", "content": "Reply with exactly the token PONG-1 and nothing else."},
                    ],
                    **openai_metadata(session),
                }
            ),
        )
        rows.append(row("normal-openai", i + 1, 1, "openai-chat", http, files, session))
        write_rows(rows)

    # Warm multi-turn
    for i in range(REPS):
        session = f"p15-w-{i}-{uuid.uuid4().hex[:8]}"
        http1, files1 = go(
            "warm-t1",
            lambda: post_claude(
                {
                    "model": MODEL,
                    "max_tokens": 64,
                    "stream": True,
                    "system": PREFIX_A,
                    "messages": [{"role": "user", "content": "Reply with exactly the token PONG-1 and nothing else."}],
                    "metadata": {"user_id": claude_user_id(session)},
                }
            ),
        )
        rows.append(row("warm", i + 1, 1, "claude-messages", http1, files1, session))
        text, _, _ = claude_text_and_tools(parse_sse_objects(http1.get("payload") or ""))
        http2, files2 = go(
            "warm-t2",
            lambda: post_claude(
                {
                    "model": MODEL,
                    "max_tokens": 64,
                    "stream": True,
                    "system": PREFIX_A,
                    "messages": [
                        {"role": "user", "content": "Reply with exactly the token PONG-1 and nothing else."},
                        {"role": "assistant", "content": text or "PONG-1"},
                        {"role": "user", "content": "Reply with exactly the token PONG-2 and nothing else."},
                    ],
                    "metadata": {"user_id": claude_user_id(session)},
                }
            ),
        )
        rows.append(row("warm", i + 1, 2, "claude-messages", http2, files2, session))
        write_rows(rows)

    # Tool + resume (1–3)
    tool_reps = min(3, REPS)
    for i in range(tool_reps):
        session = f"p15-t-{i}-{uuid.uuid4().hex[:8]}"
        http1, files1 = go(
            "tool-t1",
            lambda: post_claude(
                {
                    "model": MODEL,
                    "max_tokens": 256,
                    "stream": True,
                    "system": PREFIX_A,
                    "tools": TOOLS_CLAUDE,
                    "messages": [{"role": "user", "content": "Call echo_mock with text=hi then stop."}],
                    "metadata": {"user_id": claude_user_id(session)},
                }
            ),
        )
        rows.append(row("tool", i + 1, 1, "claude-messages", http1, files1, session))
        _, tools, _ = claude_text_and_tools(parse_sse_objects(http1.get("payload") or ""))
        if tools:
            tool = tools[0]
            http2, files2 = go(
                "tool-t2",
                lambda: post_claude(
                    {
                        "model": MODEL,
                        "max_tokens": 256,
                        "stream": True,
                        "system": PREFIX_A,
                        "tools": TOOLS_CLAUDE,
                        "messages": [
                            {"role": "user", "content": "Call echo_mock with text=hi then stop."},
                            {
                                "role": "assistant",
                                "content": [
                                    {
                                        "type": "tool_use",
                                        "id": tool.get("id"),
                                        "name": tool.get("name"),
                                        "input": tool.get("input") or {},
                                    }
                                ],
                            },
                            {
                                "role": "user",
                                "content": [
                                    {
                                        "type": "tool_result",
                                        "tool_use_id": tool.get("id"),
                                        "content": "hi",
                                    }
                                ],
                            },
                        ],
                        "metadata": {"user_id": claude_user_id(session)},
                    }
                ),
            )
            rows.append(row("resume", i + 1, 2, "claude-messages", http2, files2, session))
        write_rows(rows)

    # Cancel: short client timeout
    for i in range(min(2, REPS)):
        session = f"p15-c-{i}-{uuid.uuid4().hex[:8]}"
        http, files = go(
            "cancel",
            lambda: post_claude(
                {
                    "model": MODEL,
                    "max_tokens": 2048,
                    "stream": True,
                    "system": PREFIX_A,
                    "messages": [{"role": "user", "content": "Write a long detailed essay about rivers."}],
                    "metadata": {"user_id": claude_user_id(session)},
                },
                timeout=1,
            ),
        )
        rows.append(row("cancel", i + 1, 1, "claude-messages", http, files, session))
        write_rows(rows)

    unknown = sum(1 for r in rows if r.get("classification") in (None, "UNKNOWN"))
    invented = sum(1 for r in rows if r.get("invented_cache"))
    print(json.dumps({"n": len(rows), "unknown": unknown, "invented_cache": invented}, indent=2))
    return 0 if unknown == 0 and invented == 0 else 1


if __name__ == "__main__":
    raise SystemExit(main())
