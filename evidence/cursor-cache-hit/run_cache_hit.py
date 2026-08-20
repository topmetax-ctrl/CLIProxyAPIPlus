#!/usr/bin/env python3
"""Live Cursor Grok prompt-cache audit.

Observes TurnEndedUpdate wire fields and HTTP usage. Does not invent
cached_tokens=0 from a missing field. Continuity signals are recorded
as conditions only, never as cache proof.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid
from pathlib import Path
from typing import Any

HERE = Path(__file__).resolve().parent
REPO = HERE.parents[1]
WIRE_DIR = HERE / "wire"
FINGERPRINT_DIR = HERE / "request-fingerprints"
RAW_DIR = HERE / "raw"

BASE_URL = os.environ.get("CLIPROXY_BASE_URL", "http://127.0.0.1:8321")
API_KEY = os.environ.get("CLIPROXY_API_KEY", "audit-local-key-1")
CURSOR_MODEL = os.environ.get("CLIPROXY_CURSOR_MODEL", "cursor-grok-4.6-xhigh-fast")
XAI_MODEL = os.environ.get("CLIPROXY_XAI_MODEL", "grok-4.6")
LISTEN_PORT = int(os.environ.get("CLIPROXY_PORT", "8321"))
TIMEOUT_S = int(os.environ.get("CLIPROXY_TIMEOUT_S", "360"))

SIGNAL_NEEDLES = (
    "using saved checkpoint",
    "flattening",
    "cursor session park",
    "cursor session restore",
    "cursor session replace",
    "cold continuation",
    "auth migrated",
    "TurnEnded",
    "turn_ended",
    "TokenDeltaUpdate",
    "auth_unavailable",
)

CACHE_KEY_PATHS = (
    ("prompt_tokens_details", "cached_tokens"),
    ("input_tokens_details", "cached_tokens"),
    ("cached_tokens",),
    ("cache_read_input_tokens",),
    ("cache_read_tokens",),
)
INPUT_KEYS = ("prompt_tokens", "input_tokens")
OUTPUT_KEYS = ("completion_tokens", "output_tokens")

TOOLS_CLAUDE = [
    {
        "name": "echo_mock",
        "description": "Echo a short string. Use this instead of guessing.",
        "input_schema": {
            "type": "object",
            "properties": {"text": {"type": "string"}},
            "required": ["text"],
        },
    }
]
TOOLS_OPENAI = [
    {
        "type": "function",
        "function": {
            "name": "echo_mock",
            "description": "Echo a short string. Use this instead of guessing.",
            "parameters": {
                "type": "object",
                "properties": {"text": {"type": "string"}},
                "required": ["text"],
            },
        },
    }
]


def stable_prefix(version: str) -> str:
    unit = (
        f"SYSTEM prefix version {version}. Stable cache-hit audit prefix. "
        "Keep this block byte-identical across turns. "
        "Do not paraphrase this block. Section marker {{i:04d}}. "
    )
    return "".join(unit.format(i=i) for i in range(1, 401))


PREFIX_A = stable_prefix("A")
PREFIX_B = stable_prefix("B")
PREFIX_A_SHA = hashlib.sha256(PREFIX_A.encode()).hexdigest()
PREFIX_B_SHA = hashlib.sha256(PREFIX_B.encode()).hexdigest()


def sha256_text(text: str) -> str:
    return hashlib.sha256(text.encode()).hexdigest()


def claude_user_id(session_id: str) -> str:
    return json.dumps(
        {"session_id": session_id, "device_id": "cache-hit-audit-v2"},
        separators=(",", ":"),
    )


def openai_metadata(session_id: str) -> dict[str, str]:
    return {"user_id": claude_user_id(session_id)}


def log_size(path: str) -> int:
    try:
        return os.path.getsize(path)
    except FileNotFoundError:
        return 0


def read_log_from(path: str, start: int) -> str:
    try:
        with open(path, "rb") as f:
            f.seek(start)
            return f.read().decode("utf-8", "replace")
    except FileNotFoundError:
        return ""


def sanitize(text: str) -> str:
    text = re.sub(r"Bearer\s+[A-Za-z0-9._\-]+", "Bearer [redacted]", text)
    text = re.sub(r"sk-[A-Za-z0-9]{8,}", "sk-[redacted]", text)
    text = re.sub(
        r"(access_token|refresh_token|api[_-]?key)[=:][\s\"]*[^\s\"]+",
        r"\1=[redacted]",
        text,
        flags=re.I,
    )
    return text


def event_lines(blob: str) -> list[str]:
    out = []
    for line in blob.splitlines():
        if any(n in line for n in SIGNAL_NEEDLES):
            out.append(sanitize(line.rstrip())[:2000])
    return out


def proxy_signals(lines: list[str]) -> dict[str, Any]:
    blob = "\n".join(lines)
    return {
        "checkpoint": "using saved checkpoint" in blob,
        "flatten": "flattening" in blob,
        "park": "cursor session park" in blob,
        "restore": "cursor session restore" in blob,
        "cold_continuation": "cold continuation" in blob,
        "auth_migrated": "auth migrated" in blob,
        "turn_ended_log": "TurnEnded" in blob,
        "needles": [n for n in SIGNAL_NEEDLES if n in blob],
    }


def cached_from_usage(usage: dict[str, Any] | None) -> tuple[int | None, bool]:
    if not isinstance(usage, dict):
        return None, False
    for path in CACHE_KEY_PATHS:
        cur: Any = usage
        ok = True
        for key in path:
            if not isinstance(cur, dict) or key not in cur:
                ok = False
                break
            cur = cur[key]
        if ok and isinstance(cur, (int, float)):
            return int(cur), True
    return None, False


def token_from_usage(usage: dict[str, Any] | None, keys: tuple[str, ...]) -> int | None:
    if not isinstance(usage, dict):
        return None
    for key in keys:
        if key in usage and isinstance(usage[key], (int, float)):
            return int(usage[key])
    return None


def merge_usage(objects: list[Any]) -> dict[str, Any] | None:
    usage: dict[str, Any] | None = None

    def walk(node: Any) -> None:
        nonlocal usage
        if isinstance(node, dict):
            raw = node.get("usage")
            if isinstance(raw, dict):
                if usage is None:
                    usage = dict(raw)
                else:
                    merged = dict(usage)
                    merged.update(raw)
                    usage = merged
            for value in node.values():
                walk(value)
        elif isinstance(node, list):
            for item in node:
                walk(item)

    for obj in objects:
        walk(obj)
    return usage


def parse_sse_objects(payload: str) -> list[Any]:
    objs: list[Any] = []
    for raw in payload.splitlines():
        line = raw.strip()
        if line.startswith("data:"):
            line = line[5:].strip()
        if not line or line == "[DONE]":
            continue
        try:
            objs.append(json.loads(line))
        except json.JSONDecodeError:
            continue
    return objs


def claude_text_and_tools(objs: list[Any]) -> tuple[str, list[dict[str, Any]], str]:
    tools: list[dict[str, Any]] = []
    text: list[str] = []
    stop = ""
    current: dict[str, Any] | None = None
    json_buf = ""
    for ev in objs:
        if not isinstance(ev, dict):
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
                text.append(str(block["text"]))
        elif typ == "content_block_delta":
            delta = ev.get("delta") or {}
            if delta.get("type") == "text_delta" and delta.get("text"):
                text.append(str(delta["text"]))
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
            return "".join(text), tools, "error"
    if current is not None:
        tools.append(current)
    return "".join(text), tools, stop


def openai_text_and_tools(objs: list[Any]) -> tuple[str, list[dict[str, Any]], str]:
    text: list[str] = []
    tools: dict[int, dict[str, Any]] = {}
    stop = ""
    for ev in objs:
        if not isinstance(ev, dict):
            continue
        for choice in ev.get("choices") or []:
            if not isinstance(choice, dict):
                continue
            stop = choice.get("finish_reason") or stop
            delta = choice.get("delta") or choice.get("message") or {}
            if not isinstance(delta, dict):
                continue
            if delta.get("content"):
                text.append(str(delta["content"]))
            for tc in delta.get("tool_calls") or []:
                if not isinstance(tc, dict):
                    continue
                idx = int(tc.get("index") or 0)
                slot = tools.setdefault(idx, {"id": "", "name": "", "arguments": ""})
                if tc.get("id"):
                    slot["id"] = str(tc["id"])
                fn = tc.get("function") or {}
                if isinstance(fn, dict):
                    if fn.get("name"):
                        slot["name"] = str(fn["name"])
                    if fn.get("arguments"):
                        slot["arguments"] += str(fn["arguments"])
        message = ((ev.get("choices") or [{}])[0] or {}).get("message") or {}
        if isinstance(message, dict) and message.get("content") and not text:
            text.append(str(message["content"]))
    parsed_tools = []
    for idx in sorted(tools):
        slot = tools[idx]
        args: Any = slot["arguments"]
        try:
            args_obj = json.loads(args) if args else {}
        except json.JSONDecodeError:
            args_obj = {"raw": args}
        parsed_tools.append(
            {"id": slot["id"], "name": slot["name"], "input": args_obj, "arguments": slot["arguments"]}
        )
    return "".join(text), parsed_tools, stop


def http_stream(url: str, body: dict[str, Any], headers: dict[str, str], timeout: int) -> dict[str, Any]:
    data = json.dumps(body).encode()
    req = urllib.request.Request(url, data=data, headers=headers, method="POST")
    t0 = time.perf_counter()
    ttft_ms = None
    status = 0
    payload = ""
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            status = resp.status
            chunks: list[bytes] = []
            while True:
                raw = resp.readline()
                if not raw:
                    break
                if ttft_ms is None:
                    ttft_ms = int((time.perf_counter() - t0) * 1000)
                chunks.append(raw)
            payload = b"".join(chunks).decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        status = e.code
        payload = e.read().decode("utf-8", "replace")
        if ttft_ms is None:
            ttft_ms = int((time.perf_counter() - t0) * 1000)
    except Exception as e:
        status = 0
        payload = str(e)
        if ttft_ms is None:
            ttft_ms = int((time.perf_counter() - t0) * 1000)
    return {
        "status": status,
        "payload": payload,
        "ttft_ms": ttft_ms if ttft_ms is not None else int((time.perf_counter() - t0) * 1000),
        "wall_ms": int((time.perf_counter() - t0) * 1000),
    }


def with_retry(fn, retries: int = 1, wait_s: float = 20.0):
    result = fn()
    if result.get("status") in (429, 503) and retries > 0:
        time.sleep(wait_s)
        result = fn()
        result["retried"] = True
    return result


def git_identity() -> dict[str, Any]:
    def run(args: list[str]) -> str:
        try:
            return subprocess.check_output(args, cwd=str(REPO), text=True).strip()
        except Exception:
            return ""

    return {
        "head": run(["git", "rev-parse", "HEAD"]),
        "short": run(["git", "rev-parse", "--short", "HEAD"]),
        "branch": run(["git", "status", "-sb"]).splitlines()[0] if run(["git", "status", "-sb"]) else "",
    }


def listener_identity(port: int) -> dict[str, Any]:
    info: dict[str, Any] = {"pid": None, "command": "", "binary": "", "sha256": "", "open_logs": [], "port": port}
    try:
        out = subprocess.check_output(
            ["lsof", "-t", "-nP", f"-iTCP:{port}", "-sTCP:LISTEN"],
            text=True,
        ).strip()
        pid = out.splitlines()[0] if out else ""
        if pid:
            info["pid"] = int(pid)
            cmd = subprocess.check_output(["ps", "-p", pid, "-o", "command="], text=True).strip()
            info["command"] = cmd
            binary = cmd.split()[0] if cmd else ""
            info["binary"] = binary
            if binary and Path(binary).exists():
                info["sha256"] = hashlib.sha256(Path(binary).read_bytes()).hexdigest()
            try:
                lsof = subprocess.check_output(["lsof", "-p", pid], text=True, stderr=subprocess.DEVNULL)
                logs = sorted({line.split()[-1] for line in lsof.splitlines() if line.endswith(".log")})
                info["open_logs"] = logs
            except Exception:
                pass
    except Exception as e:
        info["error"] = str(e)
    return info


def detect_log_file(explicit: str | None, port: int) -> str:
    if explicit:
        return explicit
    env = os.environ.get("CLIPROXY_LOG")
    if env:
        return env
    logs = listener_identity(port).get("open_logs") or []
    if logs:
        return logs[0]
    return f"/tmp/cliproxy-cache-audit-{port}.log"


def get_models() -> tuple[int, list[str], str]:
    req = urllib.request.Request(
        BASE_URL.rstrip("/") + "/v1/models",
        headers={"Authorization": "Bearer " + API_KEY},
    )
    try:
        with urllib.request.urlopen(req, timeout=20) as resp:
            data = json.load(resp)
            ids = [m.get("id") for m in data.get("data", []) if m.get("id")]
            return resp.status, ids, ""
    except urllib.error.HTTPError as e:
        return e.code, [], e.read().decode("utf-8", "replace")[:500]
    except Exception as e:
        return 0, [], str(e)


class WireSink:
    def __init__(self, directory: Path) -> None:
        self.directory = directory
        self.directory.mkdir(parents=True, exist_ok=True)
        self.seen = {p.name for p in self.directory.iterdir() if p.is_file()}

    def collect_new(self) -> list[Path]:
        found: list[Path] = []
        for path in sorted(self.directory.iterdir()):
            if path.is_file() and path.name not in self.seen:
                self.seen.add(path.name)
                found.append(path)
        return found


def latest_json(files: list[Path], suffix: str) -> dict[str, Any] | None:
    matches = [p for p in files if p.name.endswith(suffix)]
    if not matches:
        return None
    try:
        return json.loads(matches[-1].read_text(encoding="utf-8"))
    except Exception:
        return None


def classify_row(
    status: int | None,
    skipped: bool,
    turn_ended: dict[str, Any] | None,
    http_cached: int | None,
    http_cached_present: bool,
) -> str:
    if skipped:
        return "SKIPPED"
    if status != 200:
        return "INCONCLUSIVE"
    if http_cached_present and http_cached is not None:
        if http_cached > 0:
            return "HIT"
        return "MISS"
    if turn_ended and turn_ended.get("varints"):
        # Field semantics are assigned after the matrix. Presence of TurnEnded
        # varints is not itself a hit; keep UNOBSERVABLE until post-process.
        return "UNOBSERVABLE"
    return "UNOBSERVABLE"


def record_row(
    rows: list[dict[str, Any]],
    excerpts: list[str],
    *,
    case_id: str,
    rep: int,
    turn: int,
    protocol: str,
    model: str,
    session_id: str,
    prefix_version: str,
    prefix: str,
    http: dict[str, Any],
    http_usage: dict[str, Any] | None,
    log_lines: list[str],
    new_files: list[Path],
    extra: dict[str, Any] | None = None,
) -> dict[str, Any]:
    cached, present = cached_from_usage(http_usage)
    signals = proxy_signals(log_lines)
    turn_ended = latest_json(new_files, "-turn_ended.json")
    fingerprint = latest_json(new_files, "-fingerprint.json")
    varints = {}
    if turn_ended and isinstance(turn_ended.get("varints"), dict):
        varints = {str(k): int(v) for k, v in turn_ended["varints"].items()}
    hex_files = [p.name for p in new_files if p.name.endswith("-turn_ended.hex")]
    json_files = [p.name for p in new_files if p.name.endswith("-turn_ended.json")]
    if fingerprint:
        dest = FINGERPRINT_DIR / f"{case_id}-r{rep}-t{turn}.json"
        dest.write_text(json.dumps(fingerprint, indent=2) + "\n", encoding="utf-8")
    usage = {
        "source": "turn_ended" if varints else ("http" if http_usage else None),
        "http_input_tokens": token_from_usage(http_usage, INPUT_KEYS),
        "http_output_tokens": token_from_usage(http_usage, OUTPUT_KEYS),
        "http_cached_tokens": cached if present else None,
        "turn_ended_varints": varints or None,
        "input_tokens": None,
        "output_tokens": None,
        "cache_read_tokens": None,
        "cache_write_tokens": None,
        "reasoning_tokens": None,
        "unknown_field_1": varints.get("1"),
        "unknown_field_2": varints.get("2"),
        "unknown_field_3": varints.get("3"),
        "unknown_field_4": varints.get("4"),
        "unknown_field_5": varints.get("5"),
    }
    row: dict[str, Any] = {
        "case": case_id,
        "rep": rep,
        "turn": turn,
        "provider": "cursor",
        "model": model,
        "protocol": protocol,
        "http_status": http.get("status"),
        "ttft_ms": http.get("ttft_ms"),
        "wall_ms": http.get("wall_ms"),
        "continuity": {
            "checkpoint": bool(signals["checkpoint"]),
            "park_restore": bool(signals["park"] or signals["restore"]),
            "cold_continuation": bool(signals["cold_continuation"]),
            "flatten": bool(signals["flatten"]),
            "mode": (fingerprint or {}).get("continuity"),
        },
        "request_fingerprint": {
            "system_sha256": (fingerprint or {}).get("system_sha256"),
            "tools_sha256": (fingerprint or {}).get("tools_sha256"),
            "history_prefix_sha256": (fingerprint or {}).get("history_prefix_sha256"),
            "user_text_sha256": (fingerprint or {}).get("user_text_sha256"),
            "stable_prefix_bytes": len(prefix.encode()),
            "stable_prefix_sha256": sha256_text(prefix),
            "prefix_version": prefix_version,
            "has_raw_checkpoint": (fingerprint or {}).get("has_raw_checkpoint"),
            "message_id_is_uuid": (fingerprint or {}).get("message_id_is_uuid"),
        },
        "usage": usage,
        "wire": {
            "turn_ended_seen": bool(varints),
            "outer_field": 14 if varints else None,
            "raw_hex_file": hex_files[-1] if hex_files else None,
            "raw_json_file": json_files[-1] if json_files else None,
        },
        "proxy_signals": signals,
        "session_id": session_id,
        "assistant_excerpt": "",
        "result": classify_row(http.get("status"), False, turn_ended, cached, present),
        "retried": bool(http.get("retried")),
        "error_excerpt": sanitize(http.get("payload", "")[:300]) if http.get("status") != 200 else "",
    }
    if extra:
        row.update(extra)
    rows.append(row)
    excerpts.append(
        f"===== {case_id} rep={rep} turn={turn} status={http.get('status')} result={row['result']} varints={varints} ====="
    )
    excerpts.extend(log_lines[:80])
    excerpts.append("")
    return row


def skipped_row(case_id: str, turn: int, protocol: str, reason: str, rep: int = 1) -> dict[str, Any]:
    return {
        "case": case_id,
        "rep": rep,
        "turn": turn,
        "provider": "cursor",
        "model": CURSOR_MODEL,
        "protocol": protocol,
        "http_status": None,
        "continuity": {
            "checkpoint": False,
            "park_restore": False,
            "cold_continuation": False,
            "flatten": False,
            "mode": None,
        },
        "request_fingerprint": {
            "system_sha256": None,
            "tools_sha256": None,
            "history_prefix_sha256": None,
            "user_text_sha256": None,
            "stable_prefix_bytes": len(PREFIX_A.encode()),
            "stable_prefix_sha256": PREFIX_A_SHA,
            "prefix_version": "A",
        },
        "usage": {
            "source": None,
            "input_tokens": None,
            "output_tokens": None,
            "cache_read_tokens": None,
            "cache_write_tokens": None,
            "reasoning_tokens": None,
        },
        "wire": {"turn_ended_seen": False, "outer_field": None, "raw_hex_file": None},
        "proxy_signals": {},
        "result": "SKIPPED",
        "skip_reason": reason,
    }


def post_claude(body: dict[str, Any], timeout: int = TIMEOUT_S) -> dict[str, Any]:
    return with_retry(
        lambda: http_stream(
            BASE_URL.rstrip("/") + "/v1/messages",
            body,
            {
                "Content-Type": "application/json",
                "Authorization": "Bearer " + API_KEY,
                "x-api-key": API_KEY,
                "anthropic-version": "2023-06-01",
            },
            timeout,
        )
    )


def post_openai(body: dict[str, Any], extra_headers: dict[str, str] | None = None, timeout: int = TIMEOUT_S) -> dict[str, Any]:
    headers = {
        "Content-Type": "application/json",
        "Authorization": "Bearer " + API_KEY,
    }
    if extra_headers:
        headers.update(extra_headers)
    return with_retry(lambda: http_stream(BASE_URL.rstrip("/") + "/v1/chat/completions", body, headers, timeout))


def wait_for_correlated_settlement(sink: WireSink, files: list[Path], timeout_s: float = 8.0):
    deadline = time.monotonic() + timeout_s
    collected = list(files)
    while True:
        fingerprint = latest_json(collected, "-fingerprint.json")
        audit = (fingerprint or {}).get("audit_id")
        settled = None
        for path in collected:
            if not path.name.endswith("-usage_settled.json"):
                continue
            try:
                data = json.loads(path.read_text(encoding="utf-8"))
            except Exception:
                continue
            if not audit or data.get("audit_id") == audit:
                settled = data
                break
        if settled is not None or time.monotonic() >= deadline:
            return collected, fingerprint, settled
        time.sleep(0.05)
        collected.extend(sink.collect_new())


def run_logged(log_path: str, sink: WireSink, fn, wait_settlement: bool = True):
    start = log_size(log_path)
    result = fn()
    blob = read_log_from(log_path, start)
    files = sink.collect_new()
    if wait_settlement:
        files, _, _ = wait_for_correlated_settlement(sink, files)
    return result, event_lines(blob), files


def preflight(log_path: str, sink: WireSink) -> dict[str, Any]:
    models_status, model_ids, models_err = get_models()
    cursor_ok = CURSOR_MODEL in model_ids
    xai_ok = XAI_MODEL in model_ids
    cursor: dict[str, Any] = {"skipped": False}
    if not cursor_ok:
        cursor = {
            "skipped": True,
            "skip_reason": f"CURSOR LIVE: SKIPPED — missing auth/model {CURSOR_MODEL} (status={models_status})",
        }
    else:
        http: dict[str, Any] = {}
        lines: list[str] = []
        for attempt in range(3):
            session = "preflight-cursor-" + uuid.uuid4().hex[:8]
            http, lines, _ = run_logged(
                log_path,
                sink,
                lambda: post_claude(
                    {
                        "model": CURSOR_MODEL,
                        "max_tokens": 32,
                        "stream": True,
                        "metadata": {"user_id": claude_user_id(session)},
                        "messages": [{"role": "user", "content": "Reply with exactly OK and nothing else."}],
                    },
                    timeout=120,
                ),
            )
            if http.get("status") == 200:
                break
            if http.get("status") not in (0, 502, 503) or attempt == 2:
                break
            time.sleep(2)
        cursor = {
            "skipped": http.get("status") != 200,
            "http_status": http.get("status"),
            "ttft_ms": http.get("ttft_ms"),
            "wall_ms": http.get("wall_ms"),
            "skip_reason": "" if http.get("status") == 200 else sanitize(http.get("payload", "")[:400]),
            "proxy_signals": proxy_signals(lines),
            "attempts": attempt + 1,
        }
    xai = {
        "skipped": True,
        "skip_reason": "XAI DIRECT: SKIPPED — missing auth"
        if not xai_ok
        else "",
    }
    if xai_ok:
        xai = {"skipped": False, "skip_reason": ""}
    return {
        "base_url": BASE_URL,
        "log_file": log_path,
        "models_http": models_status,
        "models_count": len(model_ids),
        "models_error": models_err,
        "cursor_model_present": cursor_ok,
        "xai_model_present": xai_ok,
        "cursor_grok_ids": [m for m in model_ids if "cursor-grok" in m],
        "native_grok_ids": [m for m in model_ids if m.startswith("grok-")],
        "git": git_identity(),
        "listener": listener_identity(LISTEN_PORT),
        "cursor": cursor,
        "xai": xai,
        "prefix_a_chars": len(PREFIX_A),
        "prefix_a_sha256": PREFIX_A_SHA,
        "prefix_b_sha256": PREFIX_B_SHA,
        "prefix_a_ne_b": PREFIX_A != PREFIX_B,
    }


def run_cursor_claude_turns(
    rows: list[dict[str, Any]],
    excerpts: list[str],
    log_path: str,
    sink: WireSink,
    case_id: str,
    session_id: str,
    prefix: str,
    prefix_version: str,
    turns: int,
    rep: int,
) -> None:
    messages: list[dict[str, Any]] = []
    for turn in range(1, turns + 1):
        messages.append({"role": "user", "content": f"Reply with exactly the token PONG-{turn} and nothing else."})
        body = {
            "model": CURSOR_MODEL,
            "max_tokens": 64,
            "stream": True,
            "system": prefix,
            "metadata": {"user_id": claude_user_id(session_id)},
            "messages": messages,
        }
        http, lines, files = run_logged(log_path, sink, lambda b=body: post_claude(b))
        objs = parse_sse_objects(http["payload"])
        text, _, stop = claude_text_and_tools(objs)
        record_row(
            rows,
            excerpts,
            case_id=case_id,
            rep=rep,
            turn=turn,
            protocol="claude-messages",
            model=CURSOR_MODEL,
            session_id=session_id,
            prefix_version=prefix_version,
            prefix=prefix,
            http=http,
            http_usage=merge_usage(objs),
            log_lines=lines,
            new_files=files,
            extra={"assistant_excerpt": text[:120], "stop": stop},
        )
        if http.get("status") != 200:
            return
        messages.append({"role": "assistant", "content": text or f"PONG-{turn}"})


def run_cursor_openai_turns(
    rows: list[dict[str, Any]],
    excerpts: list[str],
    log_path: str,
    sink: WireSink,
    case_id: str,
    session_id: str,
    prefix: str,
    prefix_version: str,
    turns: int,
    rep: int,
) -> None:
    messages: list[dict[str, Any]] = [{"role": "system", "content": prefix}]
    for turn in range(1, turns + 1):
        messages.append({"role": "user", "content": f"Reply with exactly the token PONG-{turn} and nothing else."})
        body = {
            "model": CURSOR_MODEL,
            "stream": True,
            "max_tokens": 64,
            "messages": messages,
            "metadata": openai_metadata(session_id),
        }
        http, lines, files = run_logged(log_path, sink, lambda b=body: post_openai(b))
        objs = parse_sse_objects(http["payload"])
        text, _, stop = openai_text_and_tools(objs)
        record_row(
            rows,
            excerpts,
            case_id=case_id,
            rep=rep,
            turn=turn,
            protocol="openai-chat",
            model=CURSOR_MODEL,
            session_id=session_id,
            prefix_version=prefix_version,
            prefix=prefix,
            http=http,
            http_usage=merge_usage(objs),
            log_lines=lines,
            new_files=files,
            extra={"assistant_excerpt": text[:120], "stop": stop},
        )
        if http.get("status") != 200:
            return
        messages.append({"role": "assistant", "content": text or f"PONG-{turn}"})


def run_cursor_claude_tool(
    rows: list[dict[str, Any]],
    excerpts: list[str],
    log_path: str,
    sink: WireSink,
    case_id: str,
    session_id: str,
    prefix: str,
    rep: int,
) -> None:
    messages: list[dict[str, Any]] = [
        {
            "role": "user",
            "content": "Call echo_mock exactly once with text=cache-hit-probe. Do not answer until you have the tool result.",
        }
    ]
    body = {
        "model": CURSOR_MODEL,
        "max_tokens": 256,
        "stream": True,
        "system": prefix,
        "metadata": {"user_id": claude_user_id(session_id)},
        "tools": TOOLS_CLAUDE,
        "messages": messages,
    }
    http, lines, files = run_logged(log_path, sink, lambda: post_claude(body))
    objs = parse_sse_objects(http["payload"])
    text, tools, stop = claude_text_and_tools(objs)
    record_row(
        rows,
        excerpts,
        case_id=case_id,
        rep=rep,
        turn=1,
        protocol="claude-messages",
        model=CURSOR_MODEL,
        session_id=session_id,
        prefix_version="A",
        prefix=prefix,
        http=http,
        http_usage=merge_usage(objs),
        log_lines=lines,
        new_files=files,
        extra={"assistant_excerpt": text[:120], "stop": stop, "tool_count": len(tools)},
    )
    if http.get("status") != 200 or not tools:
        return
    tool = tools[0]
    messages.append(
        {
            "role": "assistant",
            "content": [
                {
                    "type": "tool_use",
                    "id": tool["id"],
                    "name": tool["name"],
                    "input": tool.get("input") or {},
                }
            ],
        }
    )
    messages.append(
        {
            "role": "user",
            "content": [
                {
                    "type": "tool_result",
                    "tool_use_id": tool["id"],
                    "content": "echo:cache-hit-probe",
                }
            ],
        }
    )
    body2 = {
        "model": CURSOR_MODEL,
        "max_tokens": 64,
        "stream": True,
        "system": prefix,
        "metadata": {"user_id": claude_user_id(session_id)},
        "tools": TOOLS_CLAUDE,
        "messages": messages,
    }
    http2, lines2, files2 = run_logged(log_path, sink, lambda: post_claude(body2))
    objs2 = parse_sse_objects(http2["payload"])
    text2, _, stop2 = claude_text_and_tools(objs2)
    record_row(
        rows,
        excerpts,
        case_id=case_id,
        rep=rep,
        turn=2,
        protocol="claude-messages",
        model=CURSOR_MODEL,
        session_id=session_id,
        prefix_version="A",
        prefix=prefix,
        http=http2,
        http_usage=merge_usage(objs2),
        log_lines=lines2,
        new_files=files2,
        extra={"assistant_excerpt": text2[:120], "stop": stop2},
    )


def run_cursor_openai_tool(
    rows: list[dict[str, Any]],
    excerpts: list[str],
    log_path: str,
    sink: WireSink,
    case_id: str,
    session_id: str,
    prefix: str,
    rep: int,
) -> None:
    messages: list[dict[str, Any]] = [
        {"role": "system", "content": prefix},
        {
            "role": "user",
            "content": "Call echo_mock exactly once with text=cache-hit-probe. Do not answer until you have the tool result.",
        },
    ]
    body = {
        "model": CURSOR_MODEL,
        "stream": True,
        "max_tokens": 256,
        "tools": TOOLS_OPENAI,
        "messages": messages,
        "metadata": openai_metadata(session_id),
    }
    http, lines, files = run_logged(log_path, sink, lambda: post_openai(body))
    objs = parse_sse_objects(http["payload"])
    text, tools, stop = openai_text_and_tools(objs)
    record_row(
        rows,
        excerpts,
        case_id=case_id,
        rep=rep,
        turn=1,
        protocol="openai-chat",
        model=CURSOR_MODEL,
        session_id=session_id,
        prefix_version="A",
        prefix=prefix,
        http=http,
        http_usage=merge_usage(objs),
        log_lines=lines,
        new_files=files,
        extra={"assistant_excerpt": text[:120], "stop": stop, "tool_count": len(tools)},
    )
    if http.get("status") != 200 or not tools:
        return
    tool = tools[0]
    messages.append(
        {
            "role": "assistant",
            "content": text or None,
            "tool_calls": [
                {
                    "id": tool["id"],
                    "type": "function",
                    "function": {
                        "name": tool["name"],
                        "arguments": tool.get("arguments") or json.dumps(tool.get("input") or {}),
                    },
                }
            ],
        }
    )
    messages.append({"role": "tool", "tool_call_id": tool["id"], "content": "echo:cache-hit-probe"})
    body2 = {
        "model": CURSOR_MODEL,
        "stream": True,
        "max_tokens": 64,
        "tools": TOOLS_OPENAI,
        "messages": messages,
        "metadata": openai_metadata(session_id),
    }
    http2, lines2, files2 = run_logged(log_path, sink, lambda: post_openai(body2))
    objs2 = parse_sse_objects(http2["payload"])
    text2, _, stop2 = openai_text_and_tools(objs2)
    record_row(
        rows,
        excerpts,
        case_id=case_id,
        rep=rep,
        turn=2,
        protocol="openai-chat",
        model=CURSOR_MODEL,
        session_id=session_id,
        prefix_version="A",
        prefix=prefix,
        http=http2,
        http_usage=merge_usage(objs2),
        log_lines=lines2,
        new_files=files2,
        extra={"assistant_excerpt": text2[:120], "stop": stop2},
    )


def write_jsonl(path: Path, rows: list[dict[str, Any]]) -> None:
    with path.open("w", encoding="utf-8") as f:
        for row in rows:
            f.write(json.dumps(row, ensure_ascii=False) + "\n")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--log", default=None)
    parser.add_argument("--out", default=str(HERE / "results.jsonl"))
    parser.add_argument("--identity", default=str(HERE / "IDENTITY.json"))
    parser.add_argument("--excerpts", default=str(HERE / "log_excerpts.txt"))
    parser.add_argument("--cases", nargs="*", default=["CUR-A", "CUR-B", "CUR-C", "CUR-D", "CUR-E"])
    parser.add_argument("--reps", type=int, default=3)
    parser.add_argument("--preflight-only", action="store_true")
    args = parser.parse_args()

    for directory in (HERE, WIRE_DIR, FINGERPRINT_DIR, RAW_DIR):
        directory.mkdir(parents=True, exist_ok=True)
    args.log = detect_log_file(args.log, LISTEN_PORT)
    sink = WireSink(WIRE_DIR)
    identity = preflight(args.log, sink)
    Path(args.identity).write_text(json.dumps(identity, indent=2) + "\n", encoding="utf-8")
    print(
        json.dumps(
            {
                "preflight": {
                    "models": identity["models_count"],
                    "cursor_skipped": identity["cursor"]["skipped"],
                    "xai_skipped": identity["xai"]["skipped"],
                    "cursor_reason": identity["cursor"].get("skip_reason", ""),
                    "xai_reason": identity["xai"].get("skip_reason", ""),
                    "prefix_a_chars": identity["prefix_a_chars"],
                }
            },
            ensure_ascii=False,
        )
    )
    if args.preflight_only:
        return 0

    rows: list[dict[str, Any]] = []
    excerpts: list[str] = []
    wanted = set(args.cases)
    cursor_skip = identity["cursor"]["skipped"]
    cursor_reason = identity["cursor"].get("skip_reason") or "CURSOR LIVE: SKIPPED — missing auth"
    xai_skip = identity["xai"]["skipped"]
    xai_reason = identity["xai"].get("skip_reason") or "XAI DIRECT: SKIPPED — missing auth"

    if "XAI-A" in wanted or "XAI-B" in wanted or "XAI-C" in wanted:
        for case_id in ("XAI-A", "XAI-B", "XAI-C"):
            if case_id in wanted:
                rows.append(skipped_row(case_id, 1, "openai-chat", xai_reason if xai_skip else "not executed in this audit"))

    for rep in range(1, args.reps + 1):
        if "CUR-A" in wanted:
            if cursor_skip:
                for turn in range(1, 4):
                    rows.append(skipped_row("CUR-A", turn, "claude-messages", cursor_reason, rep))
            else:
                run_cursor_claude_turns(
                    rows, excerpts, args.log, sink, "CUR-A",
                    f"cur-a-{rep}-{uuid.uuid4().hex[:8]}", PREFIX_A, "A", 3, rep,
                )
        if "CUR-B" in wanted:
            if cursor_skip:
                for turn in range(1, 4):
                    rows.append(skipped_row("CUR-B", turn, "openai-chat", cursor_reason, rep))
            else:
                run_cursor_openai_turns(
                    rows, excerpts, args.log, sink, "CUR-B",
                    f"cur-b-{rep}-{uuid.uuid4().hex[:8]}", PREFIX_A, "A", 3, rep,
                )
        if "CUR-C" in wanted:
            if cursor_skip:
                for turn in range(1, 3):
                    rows.append(skipped_row("CUR-C", turn, "claude-messages", cursor_reason, rep))
            else:
                run_cursor_claude_tool(
                    rows, excerpts, args.log, sink, "CUR-C",
                    f"cur-c-{rep}-{uuid.uuid4().hex[:8]}", PREFIX_A, rep,
                )
        if "CUR-D" in wanted:
            if cursor_skip:
                for turn in range(1, 3):
                    rows.append(skipped_row("CUR-D", turn, "openai-chat", cursor_reason, rep))
            else:
                run_cursor_openai_tool(
                    rows, excerpts, args.log, sink, "CUR-D",
                    f"cur-d-{rep}-{uuid.uuid4().hex[:8]}", PREFIX_A, rep,
                )
        if "CUR-E" in wanted:
            if cursor_skip:
                for turn in range(1, 3):
                    rows.append(skipped_row("CUR-E", turn, "claude-messages", cursor_reason, rep))
            else:
                run_cursor_claude_turns(
                    rows, excerpts, args.log, sink, "CUR-E",
                    f"cur-e-{rep}-{uuid.uuid4().hex[:8]}", PREFIX_B, "B", 2, rep,
                )

    write_jsonl(Path(args.out), rows)
    Path(args.excerpts).write_text("\n".join(excerpts) + "\n", encoding="utf-8")
    summary: dict[str, list[dict[str, Any]]] = {}
    for row in rows:
        summary.setdefault(row["case"], []).append(
            {
                "rep": row.get("rep"),
                "turn": row["turn"],
                "result": row["result"],
                "http": row.get("http_status"),
                "varints": (row.get("usage") or {}).get("turn_ended_varints"),
            }
        )
    print(json.dumps({"wrote": args.out, "n": len(rows), "cases": summary}, ensure_ascii=False))
    return 0


if __name__ == "__main__":
    sys.exit(main())
