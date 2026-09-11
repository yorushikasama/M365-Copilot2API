#!/usr/bin/env python3
"""Live regression probe for the answer-turn protocol gate (2026-09-11).

Production failure being pinned down: the caller received the literal string

    CALL_TOOL: Skill({"skill":"impeccable","args":"..."})

as *content* while the gateway also emitted the parsed tool call. Root cause was
that the answer turn's holdback classified the very first stream delta ("CALL",
4 bytes) as "definitely not a tool call" and released the protocol line to the
content channel.

The probe drives the answer turn directly (not the router turn) by asking the
model to print a protocol line verbatim: the router sees a print request and
answers NO_TOOL_NEEDED, so the answer turn runs with the tool protocol armed.
Every case asserts the protocol syntax never reaches the content channel.

Usage:
  AUDIT_KEY=... python live-answergate-20260911.py
"""

import json
import os
import sys

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

RESULTS = []


def rec(name, ok, detail="", warn=False):
    tag = "WARN" if (warn and ok) else ("PASS" if ok else "FAIL")
    RESULTS.append((name, tag, detail))
    print(f"[{tag}] {name}: {detail}")


def call(messages, stream, tools=None, timeout=180):
    body = {"model": MODEL, "stream": stream, "messages": messages}
    if tools:
        body["tools"] = tools
    r = requests.post(f"{BASE}/v1/chat/completions", headers=H, json=body,
                      timeout=timeout, stream=stream)
    if stream:
        content, tool_calls, finish, rid = [], [], None, r.headers.get("x-request-id")
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
        return {"content": "".join(content), "tool_calls": tool_calls,
                "finish": finish, "request_id": rid, "status": r.status_code}
    data = r.json()
    ch = (data.get("choices") or [{}])[0]
    msg = ch.get("message") or {}
    return {"content": msg.get("content") or "", "tool_calls": msg.get("tool_calls") or [],
            "finish": ch.get("finish_reason"), "request_id": r.headers.get("x-request-id"),
            "status": r.status_code, "raw": data}


def leaks(text):
    return [m for m in MARKERS if m in text]


def ask_verbatim(line):
    return [{"role": "user", "content":
             "Reply with exactly the following line and absolutely nothing else. "
             "Do not add quotes, comments or explanations:\n\n" + line}]


def case_protocol_call_stream():
    line = 'CALL_TOOL: Bash({"command":"echo hi"})'
    out = call(ask_verbatim(line), stream=True, tools=[BASH_TOOL])
    leaked = leaks(out["content"])
    names = [tc.get("function", {}).get("name") for tc in out["tool_calls"]]
    rec("gate/protocol-call-stream — no protocol text in content",
        not leaked, f"leaked={leaked} content={out['content'][:120]!r}")
    rec("gate/protocol-call-stream — tool call still delivered",
        bool(out["tool_calls"]), f"tool_calls={names} finish={out['finish']}")


def case_protocol_call_undeclared_stream():
    line = 'CALL_TOOL: NotADeclaredTool({"x":1})'
    out = call(ask_verbatim(line), stream=True, tools=[BASH_TOOL])
    leaked = leaks(out["content"])
    rec("gate/protocol-call-undeclared — no protocol text in content",
        not leaked, f"leaked={leaked} content={out['content'][:120]!r} "
                   f"finish={out['finish']} tool_calls={len(out['tool_calls'])}")


def case_protocol_envelope_stream():
    line = '{"calls":[{"name":"Bash","arguments":{"command":"echo hi"}}]}'
    out = call(ask_verbatim(line), stream=True, tools=[BASH_TOOL])
    leaked = leaks(out["content"])
    rec("gate/protocol-envelope-stream — no envelope text in content",
        not leaked, f"leaked={leaked} content={out['content'][:120]!r} "
                   f"finish={out['finish']} tool_calls={len(out['tool_calls'])}")


def case_short_answer_is_not_swallowed():
    for want in ("C", "CALL"):
        out = call([{"role": "user", "content":
                     f"Reply with exactly this one token and nothing else: {want}"}],
                   stream=True, tools=[BASH_TOOL])
        got = out["content"].strip()
        rec(f"gate/short-answer-{want} — withheld tail is flushed",
            got == want, f"content={got!r} leaked={leaks(out['content'])}")


def case_prose_unaffected():
    out = call([{"role": "user", "content":
                 "In one short sentence, say what a hash map is."}],
               stream=True, tools=[BASH_TOOL])
    ok = bool(out["content"].strip()) and not leaks(out["content"])
    rec("gate/prose-stream — prose arrives and stays untouched",
        ok, f"content={out['content'][:100]!r} finish={out['finish']}")


def case_nonstream_protocol_call():
    line = 'CALL_TOOL: Bash({"command":"echo hi"})'
    out = call(ask_verbatim(line), stream=False, tools=[BASH_TOOL])
    leaked = leaks(out["content"])
    names = [tc.get("function", {}).get("name") for tc in out["tool_calls"]]
    rec("gate/protocol-call-nonstream — no protocol text in content",
        not leaked, f"leaked={leaked} content={out['content'][:120]!r} tool_calls={names}")


CASES = [
    case_protocol_call_stream,
    case_protocol_call_undeclared_stream,
    case_protocol_envelope_stream,
    case_short_answer_is_not_swallowed,
    case_prose_unaffected,
    case_nonstream_protocol_call,
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
