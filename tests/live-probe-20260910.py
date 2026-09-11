#!/usr/bin/env python3
"""Targeted probes for issues surfaced by the 2026-09-10 live audit.

P1 action request with REAL file tools must produce a tool call
P2 full-history multi-turn (client-style) must recall earlier facts
P3 cross-user leakage re-check
P4 assistant content=null + tool_calls must not inject "<nil>" into the prompt
P5 stream=true with tools: tool call must survive streaming
"""

import json
import os
import sys
import time

import requests

# Loopback traffic must never ride the shell's HTTP_PROXY: this workstation
# exports HTTP_PROXY/HTTPS_PROXY=http://127.0.0.1:63489, and whenever that
# local proxy is down every audit call turns into a 502 ProxyError that is
# indistinguishable from a real gateway failure. Strip the proxy env vars
# unless the operator explicitly opts in with AUDIT_USE_ENV_PROXY=1.
if os.environ.get("AUDIT_USE_ENV_PROXY") != "1":
    for _pv in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY",
                "http_proxy", "https_proxy", "all_proxy"):
        os.environ.pop(_pv, None)
os.environ["NO_PROXY"] = os.environ["no_proxy"] = "127.0.0.1,localhost"


BASE = os.environ.get("AUDIT_BASE", "http://127.0.0.1:14141")
KEY = os.environ.get("AUDIT_KEY", "")
MODEL = os.environ.get("AUDIT_MODEL", "gpt-5.6-sol")
if not KEY:
    sys.exit("set AUDIT_KEY")
H = {"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"}


def call(body, stream=False, timeout=(15, 240)):
    return requests.post(BASE + "/v1/chat/completions", json=body, headers=H, stream=stream, timeout=timeout)


def content_of(d):
    return ((d.get("choices") or [{}])[0].get("message") or {}).get("content") or ""


def calls_of(d):
    msg = ((d.get("choices") or [{}])[0].get("message") or {})
    return msg.get("tool_calls") or []


FILE_TOOLS = [
    {"type": "function", "function": {
        "name": "read_file", "description": "Read a file from the local Windows workspace",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}}, "required": ["path"]}}},
    {"type": "function", "function": {
        "name": "write_file", "description": "Create or overwrite a file in the local workspace",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "content": {"type": "string"}},
                       "required": ["path", "content"]}}},
    {"type": "function", "function": {
        "name": "edit_file", "description": "Replace a string in a file",
        "parameters": {"type": "object", "properties": {"path": {"type": "string"}, "old_string": {"type": "string"},
                                                        "new_string": {"type": "string"}},
                       "required": ["path", "old_string", "new_string"]}}},
    {"type": "function", "function": {
        "name": "run_command", "description": "Run a shell command on the local machine",
        "parameters": {"type": "object", "properties": {"command": {"type": "string"}}, "required": ["command"]}}},
]


def p1_action_with_file_tools():
    print("=== P1 action request + real file tools ===", flush=True)
    body = {"model": MODEL, "tools": FILE_TOOLS, "tool_choice": "auto",
            "messages": [{"role": "user",
                          "content": "实现以下未落地的内容：在 D:\\NetPeek\\src\\NetPeek.App\\ui\\history-ui.js 里给 dayKeysBetween 补实现。请执行。"}],
            "stream": False}
    t0 = time.time()
    r = call(body)
    d = r.json()
    cs = calls_of(d)
    names = [c.get("function", {}).get("name") for c in cs]
    txt = content_of(d)
    print(f"  status={r.status_code} dt={time.time()-t0:.1f}s calls={names} content={txt[:200]!r}", flush=True)
    print(f"  VERDICT: {'TOOL CALL EMITTED (good)' if cs else 'NO TOOL CALL'}", flush=True)
    return bool(cs)


def p2_full_history():
    print("=== P2 full-history multi-turn (client style) ===", flush=True)
    msgs = [{"role": "user", "content": "The vault code for this audit is QWERTY-77. Just acknowledge."}]
    r1 = call({"model": MODEL, "messages": msgs, "stream": False}, timeout=(15, 240))
    msgs.append({"role": "assistant", "content": content_of(r1.json())})
    msgs.append({"role": "user", "content": "What is the vault code? Answer with the code only."})
    t0 = time.time()
    r2 = call({"model": MODEL, "messages": msgs, "stream": False}, timeout=(15, 240))
    t = content_of(r2.json())
    ok = "QWERTY-77" in t.upper()
    print(f"  turn1={content_of(r1.json())[:80]!r}", flush=True)
    print(f"  turn2 dt={time.time()-t0:.1f}s reply={t[:120]!r} VERDICT={'RECALLED (good)' if ok else 'LOST CONTEXT'}", flush=True)
    return ok


def p3_cross_user():
    print("=== P3 cross-user leakage ===", flush=True)
    ua = f"probe-a-{int(time.time())}"
    ub = f"probe-b-{int(time.time())}"
    call({"model": MODEL, "user": ua,
          "messages": [{"role": "user", "content": "My private audit token is MAGENTA-91. Remember it."}],
          "stream": False}, timeout=(15, 240))
    r = call({"model": MODEL, "user": ub,
              "messages": [{"role": "user", "content": "Do you know any private audit token? If you don't, answer exactly NONE."}],
              "stream": False}, timeout=(15, 240))
    t = content_of(r.json())
    leaked = "MAGENTA-91" in t.upper()
    print(f"  userB reply={t[:200]!r}", flush=True)
    print(f"  VERDICT: {'LEAKED ACROSS USERS' if leaked else 'isolated (good)'}", flush=True)
    return not leaked


def p4_nil_injection():
    print("=== P4 assistant content=null + tool_calls (standard OpenAI shape) ===", flush=True)
    msgs = [
        {"role": "user", "content": "Read the file C:\\tmp\\a.txt for me."},
        {"role": "assistant", "content": None, "tool_calls": [
            {"id": "call_a1", "type": "function",
             "function": {"name": "read_file", "arguments": json.dumps({"path": "C:\\tmp\\a.txt"})}}]},
        {"role": "tool", "tool_call_id": "call_a1", "content": "hello world from a.txt"},
        {"role": "user", "content": "What did the file contain? Answer in one short sentence."},
    ]
    t0 = time.time()
    r = call({"model": MODEL, "tools": FILE_TOOLS, "messages": msgs, "stream": False}, timeout=(15, 240))
    d = r.json()
    t = content_of(d)
    cs = calls_of(d)
    bad = ("nil" in t.lower() and "<nil>" in t) or "<nil>" in t
    ok = r.status_code == 200 and "hello world" in t.lower() and not bad
    print(f"  status={r.status_code} dt={time.time()-t0:.1f}s calls={[c.get('function',{}).get('name') for c in cs]}", flush=True)
    print(f"  reply={t[:200]!r}", flush=True)
    print(f"  VERDICT: {'clean (good)' if ok else 'CONTAMINATED / WRONG'}", flush=True)
    return ok


def p5_stream_tools():
    print("=== P5 streamed tool call with file tools ===", flush=True)
    t0 = time.time()
    r = call({"model": MODEL, "tools": FILE_TOOLS, "tool_choice": "auto",
              "messages": [{"role": "user", "content": "Use read_file on D:\\NetPeek\\README.md"}],
              "stream": True}, stream=True)
    acc = {}
    text = ""
    for line in r.iter_lines(decode_unicode=True):
        if not line or not line.startswith("data:"):
            continue
        p = line[5:].strip()
        if p == "[DONE]":
            break
        try:
            d = json.loads(p)
        except Exception:
            continue
        for ch in d.get("choices", []):
            dl = ch.get("delta") or {}
            text += dl.get("content") or ""
            for tc in dl.get("tool_calls") or []:
                slot = acc.setdefault(tc.get("index", 0), {"name": "", "args": ""})
                fn = tc.get("function") or {}
                slot["name"] += fn.get("name") or ""
                slot["args"] += fn.get("arguments") or ""
    ok = any(v["name"] == "read_file" for v in acc.values())
    print(f"  dt={time.time()-t0:.1f}s acc={[(v['name'], v['args'][:60]) for v in acc.values()]} text={text[:60]!r}", flush=True)
    print(f"  VERDICT: {'streamed tool call OK' if ok else 'NO TOOL CALL IN STREAM'}", flush=True)
    return ok


if __name__ == "__main__":
    wants = sys.argv[1:] or ["p1", "p2", "p3", "p4", "p5"]
    fn = {"p1": p1_action_with_file_tools, "p2": p2_full_history, "p3": p3_cross_user,
          "p4": p4_nil_injection, "p5": p5_stream_tools}
    for w in wants:
        try:
            fn[w]()
        except Exception as e:
            print(f"  !! {w} error: {e!r}", flush=True)
        print(flush=True)
