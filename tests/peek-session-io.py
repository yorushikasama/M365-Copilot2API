#!/usr/bin/env python3
"""Dump the full model I/O of one gateway request from the client's session log.

The gateway's journal only records decision summaries (`[req-trace]`,
`[tool-router]`, `[answer-tool]`, `[router-nocalls]`) - never the request body or
the model's own text. The only place the full text exists is the client session
archive, and the join key is the gateway request id, which the client stores as
`response.headers.x-request-id`:

    ~/.zcode/cli/rollout/model-io-sess_*.jsonl

Each line is one model call: `request` (full messages) and `response`
(`finishReason`, `text`, `reasoningText`, `toolCalls`, `usage`, `headers`).

Usage:
  python tests/peek-session-io.py                      # newest session file
  python tests/peek-session-io.py <request-id> [file]
  python tests/peek-session-io.py --last 3             # last N calls of a session
"""

import glob
import json
import os
import sys

ROLLOUT = os.path.expanduser(os.path.join("~", ".zcode", "cli", "rollout"))


def newest_session():
    files = glob.glob(os.path.join(ROLLOUT, "model-io-sess_*.jsonl"))
    if not files:
        raise SystemExit(f"no session logs under {ROLLOUT}")
    return max(files, key=os.path.getmtime)


def dump(rec, show_request=False):
    resp = rec.get("response") or {}
    req = rec.get("request") or {}
    hdr = resp.get("headers") or {}
    print("x-request-id :", hdr.get("x-request-id"))
    print("finishReason :", resp.get("finishReason"))
    if show_request:
        msgs = req.get("messages") or []
        print(f"request      : {len(msgs)} messages, stream={req.get('stream')}")
        last = msgs[-1] if msgs else {}
        print("last message :", json.dumps(last, ensure_ascii=False)[:600])
    print("text         :", repr(resp.get("text")))
    if resp.get("reasoningText"):
        print("reasoning    :", repr(resp["reasoningText"])[:600])
    if resp.get("toolCalls"):
        print("toolCalls    :", json.dumps(resp["toolCalls"], ensure_ascii=False)[:1500])
    print("usage        :", json.dumps(resp.get("usage"), ensure_ascii=False))
    print("-" * 60)


def main(argv):
    args = [a for a in argv if not a.startswith("--")]
    path = args[1] if len(args) > 1 else newest_session()
    target = args[0] if args else ""
    if target == "--last":
        target = ""
    records = []
    with open(path, encoding="utf-8", errors="replace") as fh:
        for line in fh:
            if target and target not in line:
                continue
            try:
                records.append(json.loads(line))
            except json.JSONDecodeError:
                continue
    show_request = "--with-request" in argv
    if "--last" in argv:
        n = int(argv[argv.index("--last") + 1]) if argv.index("--last") + 1 < len(argv) else 3
        records = records[-n:]
    print(f"# {path}  ({len(records)} record(s))")
    for rec in records:
        dump(rec, show_request)
    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
