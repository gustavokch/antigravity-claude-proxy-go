#!/usr/bin/env python3
"""Walk a thinking tool loop through the live proxy and report, per turn,
which Anthropic content blocks came back.

A turn that returns a tool_use block without a thinking block is the shape
that used to make the next request fail with
"The `reasoning_content` in the thinking mode must be passed back to the API."

Run after installing a proxy build:
    python3 scripts/zen-reasoning-probe.py
Env: PROXY_PORT (default 8080), MODEL (default deepseek-v4.1-flash-free),
     TURNS (default 3).
"""
import json
import os
import sys
import http.client

PORT = int(os.environ.get("PROXY_PORT", "8080"))
MODEL = os.environ.get("MODEL", "deepseek-v4.1-flash-free")
TURNS = int(os.environ.get("TURNS", "3"))
TOOLS = [{
    "name": "bash",
    "description": "Run a shell command.",
    "input_schema": {
        "type": "object",
        "properties": {"cmd": {"type": "string"}},
        "required": ["cmd"],
    },
}]
WORDS = ["one", "two", "three", "four", "five", "six"]


def prompt(turn):
    return (f'Call the bash tool with cmd="echo {WORDS[turn % len(WORDS)]}". '
            "Do not answer in text.")


def post(messages, tag):
    body = json.dumps({
        "model": MODEL,
        "max_tokens": 512,
        "thinking": {"type": "enabled", "budget_tokens": 256},
        "stream": True,
        "messages": messages,
        "tools": TOOLS,
    }).encode()
    conn = http.client.HTTPConnection("127.0.0.1", PORT, timeout=120)
    conn.request("POST", "/v1/messages", body, {
        "content-type": "application/json",
        "anthropic-version": "2023-06-01",
    })
    resp = conn.getresponse()
    if resp.status != 200:
        text = resp.read()[:300].decode("utf-8", "replace")
        print(f"{tag} HTTP {resp.status}: {text}")
        return None
    blocks, kind, idx, buf, tool_block = {}, None, None, [], None
    for raw in resp:
        line = raw.decode("utf-8", "replace").strip()
        if not line.startswith("data:"):
            continue
        try:
            ev = json.loads(line[5:].strip())
        except json.JSONDecodeError:
            continue
        event = ev.get("type")
        if event == "content_block_start":
            b = ev["content_block"]
            kind, idx = b["type"], ev["index"]
            buf = [b.get("thinking", "")] if kind == "thinking" else \
                  [b.get("text", "")] if kind == "text" else []
            tool_block = b if kind == "tool_use" else None
        elif event == "content_block_delta":
            d = ev["delta"]
            buf.append(d.get("thinking") or d.get("text") or d.get("partial_json") or "")
        elif event == "content_block_stop":
            if kind == "thinking":
                blocks[idx] = {"type": "thinking", "thinking": "".join(buf), "signature": ""}
            elif kind == "text":
                blocks[idx] = {"type": "text", "text": "".join(buf)}
            elif kind == "tool_use":
                b = dict(tool_block)
                b["input"] = json.loads("".join(buf) or "{}")
                blocks[idx] = b
        elif event == "message_delta":
            print(f"{tag} usage: {json.dumps(ev.get('usage'))}")
        elif event == "error":
            print(f"{tag} STREAM ERROR: {ev}")
            return None
    content = [blocks[i] for i in sorted(blocks)]
    shape = [(b["type"], len(b.get("thinking") or b.get("text") or "")) for b in content]
    print(f"{tag} -> {shape}")
    if any(b["type"] == "tool_use" for b in content) and \
            not any(b["type"] == "thinking" for b in content):
        print(f"{tag} NOTE: tool call without reasoning — placeholder path in play")
    return content


def main():
    messages = [{"role": "user", "content": [{"type": "text", "text": prompt(0)}]}]
    for turn in range(TURNS):
        content = post(messages, f"turn{turn + 1}")
        if content is None:
            return 1
        calls = [b for b in content if b["type"] == "tool_use"]
        if not calls:
            print("model answered without a tool call; stopping")
            return 0
        messages.append({"role": "assistant", "content": content})
        messages.append({"role": "user", "content": [
            {"type": "tool_result", "tool_use_id": c["id"], "content": WORDS[turn % len(WORDS)]}
            for c in calls
        ]})
    return 0


if __name__ == "__main__":
    sys.exit(main())
