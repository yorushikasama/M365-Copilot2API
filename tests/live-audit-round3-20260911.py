#!/usr/bin/env python3
"""Round-3 multi-angle live audit for m365-copilot2api (2026-09-11).

Round 1 covered protocol/errors/content/context/tools/concurrency.
Round 2 covered endpoints/params/boundary/streamdeep/regress/sessioniso/auth/tooladv.

Round 3 opens angles neither round touched:

  xproto   cross-protocol equivalence: one prompt through chat / responses /
           anthropic must agree on content, finish mapping and tool shape
  toolx    tool-call fidelity across protocols (id echo, parallel calls,
           tool-result re-injection in each protocol's own shape)
  attach   multimodal attachment pipeline (data URL, bad mime, oversized,
           text file, mixed) and the <nil> / duplicated-payload regressions
  http     HTTP-level semantics (HEAD, OPTIONS/CORS, Accept-Encoding,
           Expect: 100-continue, keep-alive reuse, chunked body)
  usage    token accounting sanity and streamed/non-streamed agreement
  model    model resolution: /v1/models consistency, case, whitespace,
           unknown id, per-model metadata endpoint
  sec      security surface: key placement, traversal paths, header abuse,
           remote-attachment URL handling

Usage:
  AUDIT_KEY=... python live-audit-round3-20260911.py [section ...]
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
H = {"Authorization": f"Bearer {KEY}", "Content-Type": "application/json"}
AH = {"x-api-key": KEY, "anthropic-version": "2023-06-01",
      "Content-Type": "application/json"}

RESULTS = []
_LOCK = threading.Lock()


def rec(sec, name, ok, detail="", secs=None, warn=False):
    tag = "WARN" if (warn and ok) else ("PASS" if ok else "FAIL")
    with _LOCK:
        RESULTS.append((sec, name, tag, detail, secs))
        stamp = f"{secs:6.1f}s" if secs is not None else "   --  "
        print(f"[{tag}] {sec:<8} {name:<46} {stamp} {detail[:170]}", flush=True)


def safe(fn, sec, name):
    try:
        fn()
    except Exception as e:  # noqa: BLE001 - audit must survive any failure
        rec(sec, name, False, f"{type(e).__name__}: {str(e)[:150]}")


def post(path, body, stream=False, timeout=(10, 300), headers=None):
    t0 = time.time()
    r = requests.post(BASE + path, json=body, headers=headers or H,
                      stream=stream, timeout=timeout)
    return r, time.time() - t0


def get(path, timeout=(10, 60), headers=None):
    t0 = time.time()
    r = requests.get(BASE + path, headers=headers or H, timeout=timeout)
    return r, time.time() - t0


def jd(resp):
    try:
        return resp.json()
    except Exception:  # noqa: BLE001
        return {}


def chat_text(d):
    ch = (d.get("choices") or [{}])[0]
    return ((ch.get("message") or {}).get("content")) or ""


def chat_finish(d):
    return ((d.get("choices") or [{}])[0].get("finish_reason"))


# --------------------------------------------------------------------------
# xproto: one prompt through all three protocols
# --------------------------------------------------------------------------
def sec_xproto():
    prompt = "Reply with exactly the word ALPHA7 and nothing else."

    def chat_side():
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "messages": [{"role": "user",
                                                   "content": prompt}],
                     "stream": False})
        d = jd(r)
        t = chat_text(d)
        rec("xproto", "chat: content + finish + usage present",
            r.status_code == 200 and "ALPHA7" in t.upper()
            and chat_finish(d) is not None and bool(d.get("usage")),
            f"HTTP={r.status_code} finish={chat_finish(d)} usage={bool(d.get('usage'))} text={t[:30]!r}", s)

    def resp_side():
        r, s = post("/v1/responses",
                    {"model": MODEL, "input": prompt, "stream": False})
        d = jd(r)
        txt = d.get("output_text") or ""
        if not txt:
            for it in (d.get("output") or []):
                for c in (it.get("content") or []):
                    txt += c.get("text") or ""
        rec("xproto", "responses: output_text + status + usage",
            r.status_code == 200 and "ALPHA7" in txt.upper()
            and d.get("status") == "completed" and bool(d.get("usage")),
            f"HTTP={r.status_code} status={d.get('status')} usage={bool(d.get('usage'))} text={txt[:30]!r}", s)

    def anth_side():
        r, s = post("/v1/messages",
                    {"model": MODEL, "max_tokens": 64,
                     "messages": [{"role": "user", "content": prompt}]},
                    headers=AH)
        d = jd(r)
        txt = "".join(b.get("text", "") for b in (d.get("content") or [])
                      if b.get("type") == "text")
        rec("xproto", "anthropic: content block + stop_reason + usage",
            r.status_code == 200 and "ALPHA7" in txt.upper()
            and d.get("stop_reason") in ("end_turn", "stop", "max_tokens")
            and bool(d.get("usage")),
            f"HTTP={r.status_code} stop={d.get('stop_reason')} usage={bool(d.get('usage'))} text={txt[:30]!r}", s)

    def finish_map():
        # max_tokens reached must surface as a protocol-native terminal reason
        # in every protocol rather than a silent stop.
        r1, _ = post("/v1/chat/completions",
                     {"model": MODEL, "max_tokens": 8,
                      "messages": [{"role": "user",
                                    "content": "Write a 200 word essay about rivers."}],
                      "stream": False})
        f = chat_finish(jd(r1))
        r2, _ = post("/v1/messages",
                     {"model": MODEL, "max_tokens": 8,
                      "messages": [{"role": "user",
                                    "content": "Write a 200 word essay about rivers."}]},
                     headers=AH)
        stop = jd(r2).get("stop_reason")
        rec("xproto", "max_tokens terminal reason mapped (chat+anthropic)",
            f in ("length", "stop") and stop in ("max_tokens", "end_turn"),
            f"chat_finish={f} anthropic_stop={stop}", None,
            warn=(f == "stop" or stop == "end_turn"))

    for fn in (chat_side, resp_side, anth_side, finish_map):
        safe(fn, "xproto", fn.__name__)


# --------------------------------------------------------------------------
# toolx: tool-call fidelity in every protocol shape
# --------------------------------------------------------------------------
TOOLS = [
    {"type": "function",
     "function": {"name": "get_weather", "description": "look up weather",
                  "parameters": {"type": "object",
                                 "properties": {"city": {"type": "string"},
                                                "unit": {"type": "string"}},
                                 "required": ["city"]}}},
    {"type": "function",
     "function": {"name": "get_time", "description": "look up time",
                  "parameters": {"type": "object",
                                 "properties": {"tz": {"type": "string"}},
                                 "required": ["tz"]}}},
]


def sec_toolx():
    def id_shape():
        r1, s1 = post("/v1/chat/completions",
                      {"model": MODEL, "tools": TOOLS, "stream": False,
                       "messages": [{"role": "user",
                                     "content": "What is the weather in Osaka right now?"}]})
        d = jd(r1)
        ch = (d.get("choices") or [{}])[0]
        tcs = (ch.get("message") or {}).get("tool_calls") or []
        if not tcs:
            rec("toolx", "chat: tool call id/type/function shape", False,
                f"no tool call, finish={ch.get('finish_reason')} text={chat_text(d)[:60]!r}", s1)
            return
        tc = tcs[0]
        ok = (isinstance(tc.get("id"), str) and tc["id"].startswith("call_")
              and tc.get("type") == "function"
              and isinstance((tc.get("function") or {}).get("name"), str)
              and isinstance((tc.get("function") or {}).get("arguments"), str))
        args_ok = False
        try:
            args_ok = isinstance(json.loads(tc["function"]["arguments"]), dict)
        except Exception:  # noqa: BLE001
            args_ok = False
        rec("toolx", "chat: tool call id/type/function shape + json args",
            ok and args_ok,
            f"id={tc.get('id')!r} type={tc.get('type')} args_parse={args_ok}", s1)

    def echo_roundtrip():
        r1, _ = post("/v1/chat/completions",
                     {"model": MODEL, "tools": TOOLS, "stream": False,
                      "messages": [{"role": "user",
                                    "content": "What is the weather in Osaka right now?"}]})
        d1 = jd(r1)
        ch = (d1.get("choices") or [{}])[0]
        tcs = (ch.get("message") or {}).get("tool_calls") or []
        if not tcs:
            rec("toolx", "chat: tool_call_id echoed back to upstream", False,
                "no first-leg tool call")
            return
        tc = tcs[0]
        msgs = [{"role": "user", "content": "What is the weather in Osaka right now?"},
                {"role": "assistant", "content": None, "tool_calls": [tc]},
                {"role": "tool", "tool_call_id": tc["id"],
                 "content": "{\"temp_c\": 21, \"sky\": \"clear\"}"},
                {"role": "user", "content": "One short sentence: what is the temperature?"}]
        r2, s2 = post("/v1/chat/completions",
                      {"model": MODEL, "tools": TOOLS, "messages": msgs,
                       "stream": False})
        d2 = jd(r2)
        t = chat_text(d2)
        rec("toolx", "chat: tool result consumed, no id-mismatch error",
            r2.status_code == 200 and "21" in t,
            f"HTTP={r2.status_code} text={t[:70]!r}", s2)

    def anthropic_tool_shape():
        atools = [{"name": "get_weather", "description": "look up weather",
                   "input_schema": {"type": "object",
                                    "properties": {"city": {"type": "string"}},
                                    "required": ["city"]}}]
        r, s = post("/v1/messages",
                    {"model": MODEL, "max_tokens": 200, "tools": atools,
                     "messages": [{"role": "user",
                                   "content": "What is the weather in Osaka right now?"}]},
                    headers=AH)
        d = jd(r)
        blocks = d.get("content") or []
        uses = [b for b in blocks if b.get("type") == "tool_use"]
        ok = (r.status_code == 200 and uses
              and isinstance(uses[0].get("id"), str)
              and isinstance(uses[0].get("input"), dict)
              and d.get("stop_reason") in ("tool_use", "end_turn"))
        rec("toolx", "anthropic: tool_use block with dict input",
            bool(ok),
            f"HTTP={r.status_code} stop={d.get('stop_reason')} blocks={[b.get('type') for b in blocks]}", s)

    def responses_tool_shape():
        ftools = [{"type": "function", "name": "get_weather",
                   "description": "look up weather",
                   "parameters": {"type": "object",
                                  "properties": {"city": {"type": "string"}},
                                  "required": ["city"]}}]
        r, s = post("/v1/responses",
                    {"model": MODEL, "tools": ftools, "stream": False,
                     "input": "What is the weather in Osaka right now?"})
        d = jd(r)
        items = d.get("output") or []
        calls = [it for it in items if it.get("type") == "function_call"]
        ok = r.status_code == 200 and bool(calls)
        detail = f"HTTP={r.status_code} types={[it.get('type') for it in items]}"
        if calls:
            c = calls[0]
            ok = ok and isinstance(c.get("call_id"), str) and isinstance(c.get("arguments"), str) \
                and isinstance(c.get("name"), str)
            detail += f" call_id={c.get('call_id')!r} name={c.get('name')!r}"
        rec("toolx", "responses: function_call item shape", bool(ok), detail, s)

    def parallel_capacity():
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "tools": TOOLS, "stream": False,
                     "parallel_tool_calls": True,
                     "messages": [{"role": "user",
                                   "content": "Get BOTH the weather in Osaka AND the time in Asia/Tokyo in one go."}]})
        d = jd(r)
        tcs = (((d.get("choices") or [{}])[0].get("message") or {})
               .get("tool_calls")) or []
        names = [t.get("function", {}).get("name") for t in tcs]
        ids = [t.get("id") for t in tcs]
        rec("toolx", "parallel: multiple calls carry distinct ids",
            r.status_code == 200 and (len(tcs) >= 1 and len(set(ids)) == len(ids)),
            f"HTTP={r.status_code} calls={names} distinct_ids={len(set(ids)) == len(ids)}", s,
            warn=(len(tcs) < 2))

    for fn in (id_shape, echo_roundtrip, anthropic_tool_shape,
               responses_tool_shape, parallel_capacity):
        safe(fn, "toolx", fn.__name__)


# --------------------------------------------------------------------------
# attach: multimodal pipeline
# --------------------------------------------------------------------------
PNG_1PX = ("data:image/png;base64,"
           "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8"
           "z8DwHwAFAAH/q842iQAAAABJRU5ErkJggg==")


def sec_attach():
    def image_dataurl():
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "stream": False, "messages": [{
                        "role": "user", "content": [
                            {"type": "text", "content": "What colour is this pixel? One word."},
                            {"type": "image_url", "image_url": {"url": PNG_1PX}},
                        ]}]})
        d = jd(r)
        rec("attach", "chat: image_url data URL accepted, no 5xx",
            200 <= r.status_code < 300 and bool(chat_text(d) or
                                                ((d.get('choices') or [{}])[0].get('message') or {}).get('tool_calls')),
            f"HTTP={r.status_code} text={chat_text(d)[:60]!r}", s)

    def bad_mime():
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "stream": False, "messages": [{
                        "role": "user", "content": [
                            {"type": "text", "content": "Describe this."},
                            {"type": "image_url",
                             "image_url": {"url": "data:application/x-nonsense;base64,AAAA"}},
                        ]}]})
        rec("attach", "chat: unsupported mime not a 5xx",
            r.status_code < 500,
            f"HTTP={r.status_code} body={r.text[:80]!r}", s)

    def remote_url():
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "stream": False, "messages": [{
                        "role": "user", "content": [
                            {"type": "text", "content": "What is this?"},
                            {"type": "image_url",
                             "image_url": {"url": "https://example.invalid/none.png"}},
                        ]}]})
        rec("attach", "chat: unreachable remote image no 5xx",
            r.status_code < 500,
            f"HTTP={r.status_code} body={r.text[:80]!r}", s)

    def anthropic_image():
        r, s = post("/v1/messages",
                    {"model": MODEL, "max_tokens": 64, "messages": [{
                        "role": "user", "content": [
                            {"type": "text", "text": "One word: colour?"},
                            {"type": "image", "source": {
                                "type": "base64", "media_type": "image/png",
                                "data": PNG_1PX.split(",", 1)[1]}},
                        ]}]},
                    headers=AH)
        rec("attach", "anthropic: image block accepted",
            200 <= r.status_code < 300,
            f"HTTP={r.status_code} body={r.text[:80]!r}", s)

    def text_attachment_history():
        rr, ss = post("/v1/chat/completions",
                      {"model": MODEL, "stream": False, "messages": [
                          {"role": "user", "content": "Read this note: colour is VERMILION."},
                          {"role": "assistant", "content": "Noted."},
                          {"role": "user", "content": "Which colour was in the note? One word."}]})
        t = chat_text(jd(rr)).upper()
        rec("attach", "no <nil> leak on attachment-free multi-turn",
            "VERMILION" in t and "<NIL>" not in t,
            f"HTTP={rr.status_code} text={t[:60]!r}", ss)

    for fn in (image_dataurl, bad_mime, remote_url, anthropic_image,
               text_attachment_history):
        safe(fn, "attach", fn.__name__)


# --------------------------------------------------------------------------
# http: transport-level semantics
# --------------------------------------------------------------------------
def sec_http():
    def head_models():
        r = requests.head(BASE + "/v1/models", headers=H, timeout=(10, 30))
        rec("http", "HEAD /v1/models handled (no 5xx)",
            r.status_code < 500, f"HTTP={r.status_code} allow={r.headers.get('Allow')}")

    def options_preflight():
        r = requests.options(
            BASE + "/v1/chat/completions",
            headers={"Origin": "https://example.com",
                     "Access-Control-Request-Method": "POST",
                     "Access-Control-Request-Headers": "authorization,content-type"},
            timeout=(10, 30))
        acao = r.headers.get("Access-Control-Allow-Origin")
        rec("http", "CORS preflight answered or explicitly unsupported",
            r.status_code < 500,
            f"HTTP={r.status_code} acao={acao!r}", None,
            warn=(acao is None))

    def gzip_accept():
        h = dict(H)
        h["Accept-Encoding"] = "gzip, deflate"
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "stream": False,
                     "messages": [{"role": "user", "content": "Say OK"}]},
                    headers=h)
        rec("http", "Accept-Encoding gzip handled",
            r.status_code == 200 and bool(jd(r)),
            f"HTTP={r.status_code} enc={r.headers.get('Content-Encoding')!r}", s)

    def expect_continue():
        h = dict(H)
        h["Expect"] = "100-continue"
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "stream": False,
                     "messages": [{"role": "user", "content": "Say OK"}]},
                    headers=h)
        rec("http", "Expect: 100-continue handled",
            r.status_code == 200, f"HTTP={r.status_code}", s)

    def keepalive_reuse():
        sess = requests.Session()
        sess.trust_env = False
        ok = True
        detail = []
        for i in range(3):
            r = sess.post(BASE + "/v1/chat/completions",
                          json={"model": MODEL, "stream": False,
                                "messages": [{"role": "user", "content": f"Say N{i}"}]},
                          headers=H, timeout=(10, 120))
            detail.append(f"{r.status_code}")
            ok = ok and r.status_code == 200
        rec("http", "keep-alive: 3 sequential requests on one session",
            ok, f"codes={detail}")

    def chunked_body():
        payload = json.dumps({"model": MODEL, "stream": False,
                              "messages": [{"role": "user", "content": "Say CHUNKED"}]})

        def gen():
            yield payload.encode()

        r = requests.post(BASE + "/v1/chat/completions", data=gen(),
                          headers=H, timeout=(10, 120))
        rec("http", "chunked transfer-encoding request body",
            r.status_code == 200, f"HTTP={r.status_code} body={r.text[:60]!r}")

    def long_url():
        r = requests.get(BASE + "/v1/models?padding=" + "x" * 4000,
                         headers=H, timeout=(10, 30))
        rec("http", "very long query string no 5xx",
            r.status_code < 500, f"HTTP={r.status_code}")

    for fn in (head_models, options_preflight, gzip_accept, expect_continue,
               keepalive_reuse, chunked_body, long_url):
        safe(fn, "http", fn.__name__)


# --------------------------------------------------------------------------
# usage: token accounting
# --------------------------------------------------------------------------
def sec_usage():
    def nonstream_usage():
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "stream": False,
                     "messages": [{"role": "user",
                                   "content": "Count from one to five."}]})
        d = jd(r)
        u = d.get("usage") or {}
        pt, ct, tt = u.get("prompt_tokens"), u.get("completion_tokens"), u.get("total_tokens")
        ok = isinstance(pt, int) and isinstance(ct, int) and pt > 0 and ct > 0 \
            and tt == pt + ct
        rec("usage", "non-stream: prompt+completion=total tokens",
            ok, f"usage={u}", s)

    def stream_usage():
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "stream": True,
                     "stream_options": {"include_usage": True},
                     "messages": [{"role": "user", "content": "Count from one to five."}]},
                    stream=True)
        usage_frame = None
        chunks = 0
        for raw in r.iter_lines(decode_unicode=True):
            if not raw or not raw.strip().startswith("data:"):
                continue
            p = raw.strip()[5:].strip()
            if p == "[DONE]":
                continue
            try:
                obj = json.loads(p)
            except Exception:  # noqa: BLE001
                continue
            chunks += 1
            if obj.get("usage"):
                usage_frame = obj["usage"]
        rec("usage", "stream: include_usage emits a usage frame",
            usage_frame is not None and bool(usage_frame.get("total_tokens")),
            f"chunks={chunks} usage={usage_frame}", s)

    def throttle_headers():
        r, s = post("/v1/chat/completions",
                    {"model": MODEL, "stream": False,
                     "messages": [{"role": "user", "content": "Say OK"}]})
        hdrs = {k.lower(): v for k, v in r.headers.items()
                if "m365" in k.lower() or "throttl" in k.lower() or "rate" in k.lower()}
        rec("usage", "throttle/score headers surfaced to client",
            bool(hdrs), f"headers={hdrs}", s, warn=(not hdrs))

    def usage_monotonic():
        short, _ = post("/v1/chat/completions",
                        {"model": MODEL, "stream": False,
                         "messages": [{"role": "user", "content": "Say OK"}]})
        longp, _ = post("/v1/chat/completions",
                        {"model": MODEL, "stream": False,
                         "messages": [{"role": "user",
                                       "content": "Explain in 120 words why rivers meander."}]})
        a = (jd(short).get("usage") or {}).get("completion_tokens") or 0
        b = (jd(longp).get("usage") or {}).get("completion_tokens") or 0
        rec("usage", "completion tokens scale with output length",
            b > a, f"short={a} long={b}")

    for fn in (nonstream_usage, stream_usage, throttle_headers, usage_monotonic):
        safe(fn, "usage", fn.__name__)


# --------------------------------------------------------------------------
# model: model resolution
# --------------------------------------------------------------------------
def sec_model():
    def list_shape():
        r, _ = get("/v1/models")
        d = jd(r)
        data = d.get("data") or []
        ids = [m.get("id") for m in data]
        ok = r.status_code == 200 and data and all(isinstance(i, str) and i for i in ids) \
            and all(m.get("object") == "model" for m in data)
        rec("model", "/v1/models: object/data/id shape",
            ok, f"HTTP={r.status_code} n={len(ids)} sample={ids[:3]}")

    def accepted_ids():
        r, _ = get("/v1/models")
        ids = [m.get("id") for m in (jd(r).get("data") or [])]
        if not ids:
            rec("model", "advertised ids are accepted", False, "empty catalogue")
            return
        bad = []
        for mid in ids[:4]:
            rr, _ = post("/v1/chat/completions",
                         {"model": mid, "stream": False,
                          "messages": [{"role": "user", "content": "Say OK"}]})
            if rr.status_code != 200:
                bad.append((mid, rr.status_code))
        rec("model", "advertised ids are accepted by chat route",
            not bad, f"tested={ids[:4]} failures={bad}")

    def unknown_id():
        r, s = post("/v1/chat/completions",
                    {"model": "definitely-not-a-model-xyz", "stream": False,
                     "messages": [{"role": "user", "content": "Say OK"}]})
        rec("model", "unknown model id rejected or explicitly flagged",
            r.status_code >= 400,
            f"HTTP={r.status_code} body={r.text[:90]!r}", s,
            warn=(r.status_code == 200))

    def case_and_space():
        for mid, label in ((MODEL.upper(), "uppercase"),
                           (" " + MODEL + " ", "padded")):
            r, s = post("/v1/chat/completions",
                        {"model": mid, "stream": False,
                         "messages": [{"role": "user", "content": "Say OK"}]})
            rec("model", f"model id {label} resolved",
                r.status_code == 200,
                f"model={mid!r} HTTP={r.status_code}", s, warn=(r.status_code == 200))

    def per_model_meta():
        r = requests.get(BASE + f"/v1/models/{MODEL}", headers=H, timeout=(10, 30))
        rec("model", "GET /v1/models/{id} either serves or 404s cleanly",
            r.status_code in (200, 404, 405),
            f"HTTP={r.status_code} body={r.text[:70]!r}")

    def empty_model():
        r, s = post("/v1/chat/completions",
                    {"model": "", "stream": False,
                     "messages": [{"role": "user", "content": "Say OK"}]})
        rec("model", "empty model string rejected or defaulted loudly",
            r.status_code < 500,
            f"HTTP={r.status_code} body={r.text[:80]!r}", s,
            warn=(r.status_code == 200))

    for fn in (list_shape, accepted_ids, unknown_id, case_and_space,
               per_model_meta, empty_model):
        safe(fn, "model", fn.__name__)


# --------------------------------------------------------------------------
# sec: security surface
# --------------------------------------------------------------------------
def sec_sec():
    def key_in_query():
        r = requests.get(BASE + "/v1/models?api_key=" + KEY, timeout=(10, 30))
        rec("sec", "api key in query string rejected or ignored",
            r.status_code in (401, 403), f"HTTP={r.status_code}", None,
            warn=(r.status_code == 200))

    def traversal_paths():
        bad = []
        for p in ("/v1/../../etc/passwd",
                  "/v1/models/../../../etc/passwd",
                  "/..%2f..%2fetc%2fpasswd",
                  "/v1/%2e%2e/%2e%2e/etc/passwd"):
            try:
                rr = requests.get(BASE + p, headers=H, timeout=(10, 20),
                                  allow_redirects=False)
                if rr.status_code < 400 and b"root:" in rr.content:
                    bad.append((p, rr.status_code))
                elif rr.status_code >= 500:
                    bad.append((p, rr.status_code))
            except Exception as e:  # noqa: BLE001
                bad.append((p, type(e).__name__))
        rec("sec", "path traversal attempts do not leak or 5xx",
            not bad, f"issues={bad}")

    def auth_header_abuse():
        cases = {
            "double bearer": "Bearer Bearer " + KEY,
            "leading space": " " + KEY,
            "newline injected": "Bearer " + KEY + "\r\nX-Injected: 1",
            "tab separated": "Bearer\t" + KEY,
        }
        out = {}
        for label, val in cases.items():
            try:
                rr = requests.get(BASE + "/v1/models",
                                  headers={"Authorization": val}, timeout=(10, 20))
                out[label] = rr.status_code
            except Exception as e:  # noqa: BLE001
                out[label] = type(e).__name__
        ok = all(isinstance(v, int) and v < 500 for v in out.values())
        rec("sec", "malformed Authorization headers never 5xx",
            ok, f"results={out}")

    def huge_header():
        try:
            rr = requests.get(BASE + "/v1/models",
                              headers={"Authorization": "Bearer " + KEY,
                                       "X-Pad": "y" * 64000},
                              timeout=(10, 20))
            code = rr.status_code
        except Exception as e:  # noqa: BLE001
            code = type(e).__name__
        rec("sec", "64KB header rejected without 5xx",
            isinstance(code, int) and code < 500, f"result={code}")

    def method_fuzz():
        out = {}
        for m in ("PUT", "DELETE", "PATCH", "TRACE"):
            try:
                rr = requests.request(m, BASE + "/v1/chat/completions",
                                      headers=H, timeout=(10, 20))
                out[m] = rr.status_code
            except Exception as e:  # noqa: BLE001
                out[m] = type(e).__name__
        ok = all(isinstance(v, int) and 400 <= v < 500 for v in out.values())
        rec("sec", "unsupported methods get 4xx not 5xx",
            ok, f"results={out}")

    for fn in (key_in_query, traversal_paths, auth_header_abuse, huge_header,
               method_fuzz):
        safe(fn, "sec", fn.__name__)


SECTIONS = {
    "xproto": sec_xproto,
    "toolx": sec_toolx,
    "attach": sec_attach,
    "http": sec_http,
    "usage": sec_usage,
    "model": sec_model,
    "sec": sec_sec,
}


def main():
    want = sys.argv[1:] or list(SECTIONS)
    print(f"=== ROUND-3 AUDIT  base={BASE} model={MODEL} ===")
    print(f"=== sections: {want} ===")
    print()
    t0 = time.time()
    for name in want:
        fn = SECTIONS.get(name)
        if not fn:
            print(f"[SKIP] unknown section {name}")
            continue
        print(f"--- {name} ---")
        fn()
        print()
    wall = time.time() - t0

    total = len(RESULTS)
    npass = sum(1 for r in RESULTS if r[2] == "PASS")
    nwarn = sum(1 for r in RESULTS if r[2] == "WARN")
    nfail = sum(1 for r in RESULTS if r[2] == "FAIL")

    print("================ SUMMARY ================")
    print(f"total={total} pass={npass} warn={nwarn} fail={nfail} wall={wall:.0f}s")
    if nfail:
        print("\n--- FAILURES ---")
        for sec, name, tag, detail, _ in RESULTS:
            if tag == "FAIL":
                print(f"  [{sec}] {name} :: {detail[:160]}")
    if nwarn:
        print("\n--- WARNINGS ---")
        for sec, name, tag, detail, _ in RESULTS:
            if tag == "WARN":
                print(f"  [{sec}] {name} :: {detail[:160]}")
    print("\n--- PER-SECTION ---")
    for name in want:
        rs = [r for r in RESULTS if r[0] == name]
        if rs:
            print(f"  {name:<8} {sum(1 for r in rs if r[2] == 'PASS')} pass / "
                  f"{sum(1 for r in rs if r[2] == 'WARN')} warn / "
                  f"{sum(1 for r in rs if r[2] == 'FAIL')} fail")


if __name__ == "__main__":
    main()
