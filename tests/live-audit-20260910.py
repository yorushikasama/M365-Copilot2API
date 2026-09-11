#!/usr/bin/env python3
"""Live multi-dimensional audit for m365-copilot2api (2026-09-10).

Usage:
  python live-audit-20260910.py <section> [...]
Sections: protocol tools errors context concurrency content all

Env: AUDIT_BASE (default http://127.0.0.1:14141), AUDIT_KEY, AUDIT_MODEL
"""

import json
import os
import sys
import threading
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

_results = []


def rec(sec, name, ok, detail="", secs=None):
    tag = "PASS" if ok else "FAIL"
    t = f" [{secs:.1f}s]" if secs is not None else ""
    print(f"[{tag}]{t} {sec}/{name}" + (f" :: {detail}" if detail else ""), flush=True)
    _results.append({"sec": sec, "name": name, "ok": ok, "detail": detail, "secs": secs})


def post(path, body, stream=False, timeout=(10, 240), headers=None, raw=None):
    url = BASE + path
    if raw is not None:
        return requests.post(url, data=raw, headers=headers or H, stream=stream, timeout=timeout)
    return requests.post(url, json=body, headers=headers or H, stream=stream, timeout=timeout)


def sse_lines(resp):
    for line in resp.iter_lines(decode_unicode=True):
        if line:
            yield line


# ---------------------------------------------------------------- protocol
def sec_protocol():
    sec = "protocol"

    # p1 models
    t0 = time.time()
    try:
        r = requests.get(BASE + "/v1/models", headers=H, timeout=(10, 30))
        d = r.json()
        n = len(d.get("data", []))
        ids = {m.get("id") for m in d.get("data", [])}
        rec(sec, "models-list", r.status_code == 200 and n > 0 and MODEL in ids,
            f"status={r.status_code} n={n} has_model={MODEL in ids}", time.time() - t0)
    except Exception as e:
        rec(sec, "models-list", False, repr(e))

    # p2 chat non-stream shape
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "messages": [{"role": "user", "content": "Reply with exactly: PONG"}],
                  "stream": False})
        d = r.json()
        c = d.get("choices", [{}])[0]
        txt = (c.get("message") or {}).get("content") or ""
        rec(sec, "chat-nonstream", r.status_code == 200 and "PONG" in txt.upper(),
            f"status={r.status_code} id={d.get('id')} finish={c.get('finish_reason')} text={txt[:60]!r}",
            time.time() - t0)
    except Exception as e:
        rec(sec, "chat-nonstream", False, repr(e))

    # p3 chat stream: first chunk must be role-only, no leading content loss
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL,
                  "messages": [{"role": "user", "content": "Output exactly this list and nothing else:\n1. alpha\n2. bravo\n3. charlie"}],
                  "stream": True}, stream=True)
        frames = []
        first_frame = None
        text = ""
        saw_done = False
        for line in sse_lines(r):
            if not line.startswith("data:"):
                continue
            p = line[5:].strip()
            if p == "[DONE]":
                saw_done = True
                break
            try:
                d = json.loads(p)
            except Exception:
                continue
            frames.append(d)
            if first_frame is None:
                first_frame = d
            for ch in d.get("choices", []):
                text += ((ch.get("delta") or {}).get("content") or "")
        fd = ((first_frame or {}).get("choices", [{}])[0].get("delta") or {}) if first_frame else {}
        role_only = fd.get("role") == "assistant" and not fd.get("content")
        starts_ok = text.lstrip().startswith("1")
        rec(sec, "chat-stream-first-frame-role-only", role_only,
            f"first_delta_keys={sorted(fd.keys())} content={fd.get('content')!r} frames={len(frames)}",
            time.time() - t0)
        rec(sec, "chat-stream-first-char-not-lost", starts_ok and "1." in text,
            f"saw_done={saw_done} text_head={text[:70]!r}")
    except Exception as e:
        rec(sec, "chat-stream", False, repr(e))

    # p4 responses non-stream
    t0 = time.time()
    try:
        r = post("/v1/responses",
                 {"model": MODEL, "input": "Reply with exactly: RESP-OK", "stream": False})
        d = r.json()
        blob = json.dumps(d)
        rec(sec, "responses-nonstream", r.status_code == 200 and "RESP-OK" in blob,
            f"status={r.status_code} keys={sorted(d.keys())[:8]}", time.time() - t0)
    except Exception as e:
        rec(sec, "responses-nonstream", False, repr(e))

    # p5 responses stream: canonical event order
    t0 = time.time()
    try:
        r = post("/v1/responses",
                 {"model": MODEL, "input": "Say hi in three words.", "stream": True}, stream=True)
        events = []
        raw_lines = 0
        for line in sse_lines(r):
            raw_lines += 1
            if "event:" in line:
                events.append(line.split("event:", 1)[1].strip())
        need = ["response.created", "output_item.added", "content_part.added"]
        pos = []
        for n in need:
            hit = next((i for i, e in enumerate(events) if e == n or e.endswith(n)), None)
            pos.append(hit)
        order_ok = all(p is not None for p in pos) and pos == sorted(pos)
        rec(sec, "responses-stream-event-order", order_ok,
            f"events[:8]={events[:8]} total={len(events)}", time.time() - t0)
        rec(sec, "responses-stream-has-completed", "response.completed" in events,
            f"completed={'response.completed' in events}")
    except Exception as e:
        rec(sec, "responses-stream", False, repr(e))

    # p6 anthropic messages endpoint
    t0 = time.time()
    try:
        r = post("/v1/messages",
                 {"model": MODEL, "max_tokens": 64,
                  "messages": [{"role": "user", "content": "Reply with exactly: ANT-OK"}]})
        d = r.json() if r.headers.get("content-type", "").startswith("application/json") else {}
        blob = json.dumps(d)
        rec(sec, "messages-anthropic", r.status_code == 200 and "ANT-OK" in blob,
            f"status={r.status_code} type={d.get('type')} body={blob[:120]}", time.time() - t0)
    except Exception as e:
        rec(sec, "messages-anthropic", False, repr(e))


# ---------------------------------------------------------------- tools
TOOLS = [
    {"type": "function", "function": {
        "name": "get_weather", "description": "Get current weather for a city",
        "parameters": {"type": "object", "properties": {"city": {"type": "string"}},
                       "required": ["city"]}}},
    {"type": "function", "function": {
        "name": "get_time", "description": "Get current time in a timezone",
        "parameters": {"type": "object", "properties": {"tz": {"type": "string"}},
                       "required": ["tz"]}}},
]


def _calls(d):
    ch = (d.get("choices") or [{}])[0]
    msg = ch.get("message") or {}
    return msg.get("tool_calls") or [], msg.get("content") or "", ch.get("finish_reason")


def sec_tools():
    sec = "tools"

    # t1 single tool call, auto
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "tools": TOOLS, "tool_choice": "auto",
                  "messages": [{"role": "user", "content": "What is the weather in Beijing right now? Use the tool."}],
                  "stream": False})
        d = r.json()
        calls, txt, fin = _calls(d)
        names = [c.get("function", {}).get("name") for c in calls]
        rec(sec, "tool-single-auto", len(calls) >= 1 and "get_weather" in names,
            f"status={r.status_code} finish={fin} calls={names} content={txt[:80]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "tool-single-auto", False, repr(e))

    # t2 tool_choice=required
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "tools": TOOLS, "tool_choice": "required",
                  "messages": [{"role": "user", "content": "Pick whichever tool fits and call it."}],
                  "stream": False})
        d = r.json()
        calls, txt, fin = _calls(d)
        rec(sec, "tool-choice-required", len(calls) >= 1,
            f"status={r.status_code} finish={fin} n={len(calls)} content={txt[:60]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "tool-choice-required", False, repr(e))

    # t3 named function
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "tools": TOOLS, "tool_choice": {"type": "function", "function": {"name": "get_time"}},
                  "messages": [{"role": "user", "content": "Tell me the time in Asia/Shanghai."}],
                  "stream": False})
        d = r.json()
        calls, txt, fin = _calls(d)
        names = [c.get("function", {}).get("name") for c in calls]
        rec(sec, "tool-choice-named", "get_time" in names,
            f"status={r.status_code} finish={fin} calls={names} content={txt[:60]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "tool-choice-named", False, repr(e))

    # t4 parallel tools in one turn
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "tools": TOOLS, "tool_choice": "auto",
                  "messages": [{"role": "user",
                                "content": "I need two things at once: the weather in Tokyo AND the time in Asia/Tokyo. Call both tools in a single reply."}],
                  "stream": False})
        d = r.json()
        calls, txt, fin = _calls(d)
        names = [c.get("function", {}).get("name") for c in calls]
        rec(sec, "tool-parallel", len(calls) >= 2 and len(set(names)) >= 2,
            f"status={r.status_code} calls={names} content={txt[:80]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "tool-parallel", False, repr(e))

    # t5 multi-round: feed tool result back
    t0 = time.time()
    try:
        msgs = [{"role": "user", "content": "What is the weather in Shanghai? Use the tool."}]
        r = post("/v1/chat/completions", {"model": MODEL, "tools": TOOLS, "tool_choice": "auto",
                                         "messages": msgs, "stream": False})
        d = r.json()
        calls, txt, fin = _calls(d)
        if not calls:
            rec(sec, "tool-multi-round", False, f"round1 produced no tool_calls (content={txt[:80]!r})", time.time() - t0)
        else:
            c0 = calls[0]
            msgs.append({"role": "assistant", "content": txt or None, "tool_calls": calls})
            msgs.append({"role": "tool", "tool_call_id": c0.get("id", "call_1"),
                         "content": json.dumps({"city": "Shanghai", "temp_c": 26, "condition": "cloudy"})})
            r2 = post("/v1/chat/completions", {"model": MODEL, "tools": TOOLS, "tool_choice": "auto",
                                              "messages": msgs, "stream": False})
            d2 = r2.json()
            calls2, txt2, fin2 = _calls(d2)
            ok = r2.status_code == 200 and len(txt2.strip()) > 0
            rec(sec, "tool-multi-round", ok,
                f"round2 status={r2.status_code} finish={fin2} text={txt2[:100]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "tool-multi-round", False, repr(e))

    # t6 action-request must not degrade to prose denial (2026-09-10 regression)
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "tools": TOOLS, "tool_choice": "auto",
                  "messages": [{"role": "user",
                                "content": "实现以下未落地的内容：给 dayKeysBetween 补实现，并在 styles.css 加上自定义日期表单样式。请执行。"}],
                  "stream": False})
        d = r.json()
        calls, txt, fin = _calls(d)
        low = txt or ""
        denial = any(k in low for k in ["没有工具", "没有可调用", "无工具可用", "没有文件编辑",
                                       "no tools available", "no callable tool", "cannot edit files"])
        ok = len(calls) >= 1 and not denial
        rec(sec, "action-request-not-refused", ok,
            f"status={r.status_code} calls={len(calls)} denial={denial} text={low[:110]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "action-request-not-refused", False, repr(e))

    # t7 tool schema discipline: model must not invent argument names
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "tools": TOOLS, "tool_choice": "auto",
                  "messages": [{"role": "user", "content": "Use get_weather for Paris."}],
                  "stream": False})
        d = r.json()
        calls, txt, fin = _calls(d)
        bad = []
        for c in calls:
            fn = c.get("function", {})
            if fn.get("name") == "get_weather":
                try:
                    args = json.loads(fn.get("arguments") or "{}")
                except Exception:
                    bad.append("unparseable-args")
                    continue
                if "city" not in args:
                    bad.append(f"missing-city:{list(args)}")
        rec(sec, "tool-arg-schema", len(calls) >= 1 and not bad,
            f"calls={[c.get('function',{}).get('name') for c in calls]} problems={bad}", time.time() - t0)
    except Exception as e:
        rec(sec, "tool-arg-schema", False, repr(e))

    # t8 streamed tool call
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "tools": TOOLS, "tool_choice": "auto",
                  "messages": [{"role": "user", "content": "Weather in Berlin? Use the tool."}],
                  "stream": True}, stream=True)
        acc = {}
        text = ""
        done = False
        for line in sse_lines(r):
            if not line.startswith("data:"):
                continue
            p = line[5:].strip()
            if p == "[DONE]":
                done = True
                break
            try:
                d = json.loads(p)
            except Exception:
                continue
            for ch in d.get("choices", []):
                dl = ch.get("delta") or {}
                text += dl.get("content") or ""
                for tc in dl.get("tool_calls") or []:
                    i = tc.get("index", 0)
                    slot = acc.setdefault(i, {"name": "", "args": ""})
                    fn = tc.get("function") or {}
                    slot["name"] += fn.get("name") or ""
                    slot["args"] += fn.get("arguments") or ""
        rec(sec, "tool-streamed", any(v["name"] == "get_weather" for v in acc.values()),
            f"done={done} acc={[(v['name'], v['args'][:40]) for v in acc.values()]} text={text[:60]!r}",
            time.time() - t0)
    except Exception as e:
        rec(sec, "tool-streamed", False, repr(e))


# ---------------------------------------------------------------- errors
def sec_errors():
    sec = "errors"

    def case(name, path, body, hdr=None, expect=(400, 401, 403, 404, 422), raw=None):
        t0 = time.time()
        try:
            r = post(path, body, headers=hdr, raw=raw, timeout=(10, 120))
            ct = r.headers.get("content-type", "")
            ok = r.status_code in expect
            rec(sec, name, ok, f"status={r.status_code} ct={ct[:30]} body={r.text[:140]!r}", time.time() - t0)
        except Exception as e:
            rec(sec, name, False, repr(e))

    case("no-auth", "/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "user", "content": "hi"}], "stream": False},
         hdr={"Content-Type": "application/json"})
    case("bad-key", "/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "user", "content": "hi"}], "stream": False},
         hdr={"Authorization": "Bearer m365_deadbeef", "Content-Type": "application/json"})
    case("unknown-model", "/v1/chat/completions",
         {"model": "no-such-model-xyz", "messages": [{"role": "user", "content": "hi"}], "stream": False},
         expect=(400, 404, 422, 500))
    case("empty-messages", "/v1/chat/completions",
         {"model": MODEL, "messages": [], "stream": False})
    case("missing-messages", "/v1/chat/completions",
         {"model": MODEL, "stream": False})
    case("missing-model", "/v1/chat/completions",
         {"messages": [{"role": "user", "content": "hi"}], "stream": False})
    case("malformed-json", "/v1/chat/completions", None, raw="{not json")
    case("null-content", "/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "user", "content": None}], "stream": False})
    case("orphan-tool-msg", "/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "tool", "tool_call_id": "nope", "content": "x"}], "stream": False},
         expect=(200, 400, 422, 500))
    case("bad-role", "/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "wizard", "content": "hi"}], "stream": False},
         expect=(200, 400, 422, 500))
    case("negative-max-tokens", "/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "user", "content": "hi"}], "max_tokens": -5, "stream": False},
         expect=(200, 400, 422, 500))
    case("huge-max-tokens", "/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "user", "content": "hi"}], "max_tokens": 99999999, "stream": False},
         expect=(200, 400, 422, 500))
    # tool_choice=required with zero tools
    case("required-without-tools", "/v1/chat/completions",
         {"model": MODEL, "messages": [{"role": "user", "content": "hi"}], "tool_choice": "required", "stream": False},
         expect=(200, 400, 422, 500))
    # unknown route
    case("unknown-route", "/v1/nonexistent/endpoint",
         {"model": MODEL, "messages": [{"role": "user", "content": "hi"}]}, expect=(404, 405, 400))

    # oversized single message (~300KB)
    t0 = time.time()
    try:
        big = "x" * 300000
        r = post("/v1/chat/completions",
                 {"model": MODEL, "messages": [{"role": "user", "content": "只回复 OK。忽略后面的填充：" + big}],
                  "stream": False}, timeout=(20, 240))
        rec(sec, "oversized-message-300kb", r.status_code in (200, 400, 413, 422),
            f"status={r.status_code} body={r.text[:100]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "oversized-message-300kb", False, repr(e))


# ---------------------------------------------------------------- context
def sec_context():
    sec = "context"

    # c1 same-user multi-turn keeps context
    t0 = time.time()
    try:
        u = f"audit-{int(time.time())}"
        r1 = post("/v1/chat/completions",
                  {"model": MODEL, "user": u,
                   "messages": [{"role": "user", "content": "Remember the codeword BANANA7."}], "stream": False})
        r2 = post("/v1/chat/completions",
                  {"model": MODEL, "user": u,
                   "messages": [{"role": "user", "content": "What was the codeword I told you?"}], "stream": False})
        t2 = ((r2.json().get("choices") or [{}])[0].get("message") or {}).get("content") or ""
        rec(sec, "session-recall", "BANANA7" in t2.upper(),
            f"status={r2.status_code} reply={t2[:110]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "session-recall", False, repr(e))

    # c2 fresh user must NOT inherit previous user's memory
    t0 = time.time()
    try:
        u = f"audit-iso-{int(time.time())}"
        post("/v1/chat/completions",
             {"model": MODEL, "user": u,
              "messages": [{"role": "user", "content": "My secret project is called FALCON-42."}], "stream": False})
        u2 = f"audit-iso2-{int(time.time())}"
        r2 = post("/v1/chat/completions",
                  {"model": MODEL, "user": u2,
                   "messages": [{"role": "user", "content": "Do you know any secret project name? Answer 'none' if unknown."}],
                   "stream": False})
        t2 = ((r2.json().get("choices") or [{}])[0].get("message") or {}).get("content") or ""
        leaked = "FALCON-42" in t2.upper() or "FALCON" in t2.upper()
        rec(sec, "cross-user-isolation", not leaked, f"reply={t2[:110]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "cross-user-isolation", False, repr(e))

    # c3 new question must not be answered with old question's content
    t0 = time.time()
    try:
        u = f"audit-stale-{int(time.time())}"
        post("/v1/chat/completions",
             {"model": MODEL, "user": u,
              "messages": [{"role": "user", "content": "列出中国的三大运营商名称。"}], "stream": False})
        r2 = post("/v1/chat/completions",
                  {"model": MODEL, "user": u,
                   "messages": [{"role": "user", "content": "现在换一个话题：3 的平方根约等于多少？只回答数字。"}],
                   "stream": False})
        t2 = ((r2.json().get("choices") or [{}])[0].get("message") or {}).get("content") or ""
        stale = ("移动" in t2 and "联通" in t2)
        rec(sec, "no-stale-answer", (not stale) and ("1.7" in t2 or "1,7" in t2 or "平方根" in t2 or "√" in t2),
            f"reply={t2[:110]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "no-stale-answer", False, repr(e))

    # c4 long history (compaction-ish) still answers the last question
    t0 = time.time()
    try:
        msgs = []
        for i in range(24):
            msgs.append({"role": "user", "content": f"第{i}条历史：这是第{i}号测试消息，内容是无关的填充文本。"})
            msgs.append({"role": "assistant", "content": f"已记录第{i}号测试消息。"})
        msgs.append({"role": "user", "content": "只回答：ZEBRA99"})
        r = post("/v1/chat/completions", {"model": MODEL, "messages": msgs, "stream": False}, timeout=(10, 240))
        t = ((r.json().get("choices") or [{}])[0].get("message") or {}).get("content") or ""
        rec(sec, "long-history-tail", "ZEBRA99" in t.upper(),
            f"status={r.status_code} msgs={len(msgs)} reply={t[:100]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "long-history-tail", False, repr(e))

    # c5 sessions endpoint
    t0 = time.time()
    try:
        r = requests.get(BASE + "/v1/sessions", headers=H, timeout=(10, 60))
        rec(sec, "sessions-list", r.status_code in (200, 404, 405),
            f"status={r.status_code} body={r.text[:120]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "sessions-list", False, repr(e))


# ---------------------------------------------------------------- concurrency
def sec_concurrency(n=6):
    sec = "concurrency"
    out = []
    lock = threading.Lock()

    def worker(i):
        t0 = time.time()
        try:
            r = post("/v1/chat/completions",
                     {"model": MODEL, "user": f"cc-{i}-{int(time.time())}",
                      "messages": [{"role": "user", "content": f"Reply with exactly: C{i}"}], "stream": False},
                     timeout=(15, 240))
            txt = ((r.json().get("choices") or [{}])[0].get("message") or {}).get("content") or ""
            with lock:
                out.append((i, r.status_code, time.time() - t0, txt[:40]))
        except Exception as e:
            with lock:
                out.append((i, "EXC", time.time() - t0, repr(e)[:80]))

    t0 = time.time()
    ths = [threading.Thread(target=worker, args=(i,)) for i in range(n)]
    for t in ths:
        t.start()
    for t in ths:
        t.join()
    wall = time.time() - t0
    okn = sum(1 for o in out if o[1] == 200)
    rec(sec, f"parallel-{n}", okn == n,
        f"wall={wall:.1f}s ok={okn}/{n} detail={sorted(out, key=lambda x: str(x[0]))}",
        wall)


# ---------------------------------------------------------------- content
def sec_content():
    sec = "content"

    # n1 unicode round trip
    t0 = time.time()
    try:
        probe = "中文·emoji🎯·math∑·quote「」"
        r = post("/v1/chat/completions",
                 {"model": MODEL, "messages": [{"role": "user", "content": f"原样重复这句话，不要加解释：{probe}"}],
                  "stream": False})
        t = ((r.json().get("choices") or [{}])[0].get("message") or {}).get("content") or ""
        rec(sec, "unicode-roundtrip", "emoji🎯" in t and "中文" in t, f"reply={t[:100]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "unicode-roundtrip", False, repr(e))

    # n2 long output not truncated mid-stream
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL,
                  "messages": [{"role": "user", "content": "从 1 数到 60，每行一个数字，不要省略，不要额外解释。"}],
                  "stream": True}, stream=True)
        text = ""
        fin = None
        for line in sse_lines(r):
            if not line.startswith("data:"):
                continue
            p = line[5:].strip()
            if p == "[DONE]":
                break
            try:
                d = json.loads(p)
            except Exception:
                continue
            for ch in d.get("choices", []):
                text += ((ch.get("delta") or {}).get("content") or "")
                if ch.get("finish_reason"):
                    fin = ch["finish_reason"]
        has60 = "60" in text
        rec(sec, "long-stream-tail-intact", has60 and len(text) > 60,
            f"finish={fin} len={len(text)} tail={text[-60:]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "long-stream-tail-intact", False, repr(e))

    # n3 markdown formatting preserved (prior complaint: summaries came back non-markdown)
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL,
                  "messages": [{"role": "user",
                                "content": "用 markdown 输出一个二级标题 '## 测试' 加一个三项无序列表，不要额外说明。"}],
                  "stream": False})
        t = ((r.json().get("choices") or [{}])[0].get("message") or {}).get("content") or ""
        rec(sec, "markdown-preserved", "##" in t and ("- " in t or "* " in t or "1." in t),
            f"reply={t[:120]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "markdown-preserved", False, repr(e))

    # n4 stream aborted by client mid-flight (server must not wedge)
    t0 = time.time()
    try:
        r = post("/v1/chat/completions",
                 {"model": MODEL, "messages": [{"role": "user", "content": "写一段 800 字的中文散文。"}],
                  "stream": True}, stream=True, timeout=(10, 60))
        got = 0
        for line in sse_lines(r):
            got += 1
            if got >= 3:
                r.close()
                break
        time.sleep(1)
        r2 = post("/v1/chat/completions",
                  {"model": MODEL, "messages": [{"role": "user", "content": "Reply with exactly: AFTER-ABORT"}],
                   "stream": False}, timeout=(15, 180))
        t2 = ((r2.json().get("choices") or [{}])[0].get("message") or {}).get("content") or ""
        rec(sec, "abort-then-recover", "AFTER-ABORT" in t2.upper(),
            f"aborted_after={got} lines, followup status={r2.status_code} reply={t2[:60]!r}", time.time() - t0)
    except Exception as e:
        rec(sec, "abort-then-recover", False, repr(e))


SECTIONS = {
    "protocol": sec_protocol,
    "tools": sec_tools,
    "errors": sec_errors,
    "context": sec_context,
    "concurrency": sec_concurrency,
    "content": sec_content,
}

if __name__ == "__main__":
    want = sys.argv[1:] or ["all"]
    if want == ["all"]:
        want = ["protocol", "errors", "content", "context", "tools", "concurrency"]
    print(f"### AUDIT base={BASE} model={MODEL} sections={want}", flush=True)
    for s in want:
        f = SECTIONS.get(s)
        if not f:
            print(f"!! unknown section {s}")
            continue
        print(f"\n--- section {s} ---", flush=True)
        try:
            f()
        except Exception as e:
            print(f"[SECTION-ERROR] {s}: {e!r}", flush=True)
    npass = sum(1 for r in _results if r["ok"])
    print(f"\n### SUMMARY pass={npass} fail={len(_results) - npass} total={len(_results)}", flush=True)
    for r in _results:
        if not r["ok"]:
            print(f"  FAILED: {r['sec']}/{r['name']} :: {r['detail']}", flush=True)
