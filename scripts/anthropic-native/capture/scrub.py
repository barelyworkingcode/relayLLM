#!/usr/bin/env python3
"""Deterministic scrubber for captured request bodies (stdlib only).

Usage: scrub.py IN.jsonl OUT.jsonl

Every JSON string value is replaced by a synthetic string derived from the
SHA-256 of the original (same input, same output, so a prefix shared between
turns stays shared). Kept as-is: values of the structural keys type, role,
id, tool_use_id, name, model, media_type, ttl (cache_control ttl). Numbers,
bools, null, key order and object keys (including tool input_schema property
names) are untouched. "signature" values become synthetic base64. Length
class: exact up to 32 chars, else rounded up to a power of two, capped at
2048 chars to keep the fixture small. Output is compact JSON, one per line.
"""
import base64
import hashlib
import json
import sys
from collections import Counter

KEEP = {"type", "role", "id", "tool_use_id", "name", "model", "media_type", "ttl"}
CAP = 2048


def target_len(n: int) -> int:
    if n <= 32:
        return n
    p = 32
    while p < n:
        p *= 2
    return min(p, CAP)


def stream(seed: bytes, n: int) -> bytes:
    out, i = b"", 0
    while len(out) < n:
        out += hashlib.sha256(seed + i.to_bytes(4, "big")).digest()
        i += 1
    return out[:n]


def synth_text(s: str) -> str:
    n = target_len(len(s))
    raw = stream(b"text:" + s.encode(), n).hex()  # hex chars, one per byte
    words, i, out = [], 0, ""
    while len(out) < n:  # words of 3..8 chars from the hash stream
        w = 3 + int(raw[i % len(raw)], 16) % 6
        out += raw[i:i + w] + " "
        i += w
        if i >= len(raw):
            raw += stream(raw.encode(), n).hex()
    return out[:n].rstrip().ljust(n, "x") if n else ""


def synth_sig(s: str) -> str:
    n = max(4, target_len(len(s)) // 4 * 4)
    b = base64.b64encode(stream(b"sig:" + s.encode(), n)).decode()
    return b[:n]


def scrub(v, key=None, stats=None):
    if isinstance(v, dict):
        return {k: scrub(x, k, stats) for k, x in v.items()}
    if isinstance(v, list):
        return [scrub(x, key, stats) for x in v]
    if isinstance(v, str):
        if key in KEEP:
            return v
        return synth_sig(v) if key == "signature" else synth_text(v)
    return v


def summarize(body, counts, turn):
    for m in body.get("messages", []):
        c = m.get("content")
        if isinstance(c, str):
            counts["text(str)"] += 1
            continue
        for b in c or []:
            counts[b.get("type", "?")] += 1
    turn.append(len(body.get("messages", [])))


def main():
    if len(sys.argv) != 3:
        sys.exit(__doc__)
    counts, turns = Counter(), []
    n = 0
    with open(sys.argv[1], encoding="utf-8") as fin, open(sys.argv[2], "w", encoding="utf-8") as fout:
        for line in fin:
            if not line.strip():
                continue
            body = json.loads(line)
            out = scrub(body)
            fout.write(json.dumps(out, separators=(",", ":"), ensure_ascii=False) + "\n")
            summarize(out, counts, turns)
            n += 1
    print(f"turns: {n}; messages per turn: {turns}")
    print("block types in messages (cumulative across turns):", dict(counts))


if __name__ == "__main__":
    main()
