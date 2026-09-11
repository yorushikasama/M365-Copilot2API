#!/usr/bin/env python3
"""Live regression probe for the answer-turn protocol gate (2026-09-11).

Production failure: the caller received the literal string

    CALL_TOOL: Skill({"skill":"impeccable","args":"..."})

as *content* while the gateway also emitted the parsed tool call. Root cause:
the answer turn's holdback classified the first stream delta ("CALL", 4 bytes)
as "definitely not a tool call" and released the protocol line immediately.

Driving the answer turn deterministically:

  * `tool_choice:"none"` skips the router turn entirely (every router branch is
    gated on `fmt.Sprint(body.ToolChoice) != "none"`), so the request always
    lands on the answer turn — the only turn that can leak the protocol.
  * The prompt therefore only has to make the model *want* to call a tool. The
    marker itself never appears in the prompt: asking the model to print it
    verbatim trips M365's injection guard and yields a canned refusal, which
    proves nothing.

What is asserted is the invariant the user cares about: protocol syntax must
never appear on the content channel. Whether the model actually produced a
marker is confirmed from the gateway journal afterwards
(`[answer-tool] decision=deferred_call stream_suppressed=true` — a log line that
had never fired before this fix).

Usage:
  AUDIT_KEY=... python live-answergate-20260911.py
"""

import json
import os
import sys
import time

import requests

# Loopback traffic must never ride the shell's HTTP_PROXY (see the audit tools).
if os.environ.get("AUDIT_USE_ENV_PROXY") != "1":
    for _pv in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                "http_proxy", "https_proxy", "all_proxy"):
        os.environ.pop(_pv, None)
os.environ["NO_PROXY"] = os.environ["no_proxy"] = "127.0.0.1,localhost"

BASE = os.environ.get("AUDIT_BASE", "http://127.0.0.1:14141")
KEY = os.environ.get("AUDIT_KEY", "")
MODEL = os.environ.get("AUDIT_MODEL", "gpt-5.6-sol")
ATTEMPTS = int(os.environ.get("AUDIT_ATTEMPTS", "3"))
H = {"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"}

MARKERS = ("CALL_TOOL:", "call_tool:", '{"calls"', "CALL_TOOL：")

BASH_TOOL = {
    "type": "function",
    "function": {
        "name": "Bash",
        "description": "Run a PowerShell command on the caller's Windows machine.",
        "parameters": {
            "type": "object",
            "properties": {"command": {"type": "string"}},
            "required": ["command"],
        },
    },
}

ACTION_PROMPT = ("用 Bash 工具在这台 Windows 机器上执行 dir 命令，"
                 "然后把输出里的文件名列表告诉我。")

RESULTS = []


def rec(name, ok, detail="", warn=False):
    tag = "WARN" if (warn and ok) else ("PASS" if ok else "FAIL")
    RESULTS.append((name, tag, detail))
    print(f"[{tag}] {name}: {detail}")


def chat(messages, stream=True, tools=None, tool_choice=None, timeout=180):
    body = {"model": MODEL, "stream": stream, "messages": messages}
    if tools:
        body["tools"] = tools
    if tool_choice is not None:
        body["tool_choice"] = tool_choice
    r = requests.post(f"{BASE}/v1/chat/completions", headers=H, json=body,
                      timeout=timeout, stream=stream)
    if stream:
        content, tool_calls, finish = [], [], None
        for line in r.iter_lines(decode_unicode=True):
            if not line or not line.startswith("data: "):
                continue
            payload = line[6:]
            if payload == "[DONE]":
                break
            try:
                ev = json.loads(payload)
            except json.JSONDecodeError:
                continue
            for ch in ev.get("choices") or []:
                d = ch.get("delta") or {}
                if d.get("content"):
                    content.append(d["content"])
                if d.get("tool_calls"):
                    tool_calls.extend(d["tool_calls"])
                if ch.get("finish_reason"):
                    finish = ch["finish_reason"]
        return {"content": "".join(content), "tool_calls": tool_calls, "finish": finish,
                "status": r.status_code, "request_id": r.headers.get("x-request-id")}
    data = r.json()
    ch = (data.get("choices") or [{}])[0]
    msg = ch.get("message") or {}
    return {"content": msg.get("content") or "", "tool_calls": msg.get("tool_calls") or [],
            "finish": ch.get("finish_reason"), "status": r.status_code,
            "request_id": r.headers.get("x-request-id")}


def leaks(text):
    return [m for m in MARKERS if m in text]


def case_answer_turn_never_leaks():
    """tool_choice=none forces the answer turn; the router cannot rescue it."""
    leaked_all, seen_marker, sample = [], False, ""
    for i in range(ATTEMPTS):
        out = chat([{"role": "user", "content": ACTION_PROMPT}], tools=[BASH_TOOL],
                   tool_choice="none")
        found = leaks(out["content"])
        if found:
            leaked_all.append((i, found, out["content"][:200]))
        if out["content"]:
            sample = out["content"][:120]
        if found:
            seen_marker = True
        time.sleep(1)
    rec("gate/answer-turn(tool_choice=none) — protocol text never in content",
        not leaked_all, f"attempts={ATTEMPTS} leaked={leaked_all} sample={sample!r}")
    return seen_marker


def case_auto_router_or_answer():
    """Default path: the router may answer, the answer turn may take over."""
    leaked_all, calls_seen, samples = [], 0, []
    for i in range(ATTEMPTS):
        out = chat([{"role": "user", "content": ACTION_PROMPT}], tools=[BASH_TOOL])
        found = leaks(out["content"])
        if found:
            leaked_all.append((i, found, out["content"][:200]))
        if out["tool_calls"]:
            calls_seen += 1
        samples.append((out["finish"], len(out["tool_calls"]), out["content"][:60]))
        time.sleep(1)
    rec("gate/auto-path — protocol text never in content",
        not leaked_all, f"leaked={leaked_all}")
    rec("gate/auto-path — tool delivery still works",
        calls_seen > 0, f"tool_calls in {calls_seen}/{ATTEMPTS} attempts: {samples}")


def case_prose_unaffected():
    out = chat([{"role": "user", "content": "用一句话解释什么是哈希表。"}],
               tools=[BASH_TOOL])
    ok = bool(out["content"].strip()) and not leaks(out["content"])
    rec("gate/prose — prose arrives and stays untouched",
        ok, f"content={out['content'][:100]!r} finish={out['finish']}")


def case_short_answer_is_not_swallowed():
    """The gate holds a rolling tail; a tiny answer must still be delivered."""
    for want in ("C", "OK"):
        out = chat([{"role": "user", "content":
                     f"只回复这两个字符：{want}。不要任何其他内容、不要标点。"}],
                   tools=[BASH_TOOL], tool_choice="none")
        got = out["content"].strip()
        rec(f"gate/short-answer-{want} — withheld tail is flushed",
            want in got, f"content={got!r} leaked={leaks(out['content'])}",
            warn=(want not in got))


def case_nonstream():
    out = chat([{"role": "user", "content": ACTION_PROMPT}], stream=False,
               tools=[BASH_TOOL])
    leaked = leaks(out["content"])
    names = [tc.get("function", {}).get("name") for tc in out["tool_calls"]]
    rec("gate/non-stream — protocol text never in content",
        not leaked, f"leaked={leaked} content={out['content'][:100]!r} tool_calls={names}")


CASES = [
    case_answer_turn_never_leaks,
    case_auto_router_or_answer,
    case_prose_unaffected,
    case_short_answer_is_not_swallowed,
    case_nonstream,
]


def main():
    if not KEY:
        print("AUDIT_KEY is required", file=sys.stderr)
        return 2
    only = sys.argv[1:]
    for case in CASES:
        if only and not any(o in case.__name__ for o in only):
            continue
        try:
            case()
        except Exception as exc:  # noqa: BLE001 - a probe must never abort the run
            rec(case.__name__, False, f"exception: {exc!r}")
    failed = [r for r in RESULTS if r[1] == "FAIL"]
    print(f"\n=== {len(RESULTS) - len(failed)}/{len(RESULTS)} passed ===")
    return 1 if failed else 0


if __name__ == "__main__":
    raise SystemExit(main())
