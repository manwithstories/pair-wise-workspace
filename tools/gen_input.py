#!/usr/bin/env python3
"""Generate a synthetic SCADA gateway log for testing.

Produces JSON Lines with realistic warts: fragmented frames, out-of-order
delivery, bad base64, truncated frames, CRC failures, and stray garbage.
"""

from __future__ import annotations

import argparse
import base64
import json
import os
import random
import struct
import sys

sys.path.insert(0, os.path.dirname(os.path.dirname(os.path.abspath(__file__))))

from telem_replay.protocol import build_frame, build_fragments  # noqa: E402


def tlv(tag: int, value: bytes) -> bytes:
    return struct.pack("<HH", tag, len(value)) + value


def make_body(rng: random.Random, seq: int) -> bytes:
    out = b""
    for point in range(rng.randint(1, 4)):
        out += tlv(0x0100 | point, struct.pack("<h", rng.randint(-400, 400)))
    if rng.random() < 0.35:
        out += tlv(0x0201, bytes([rng.randint(0, 4), rng.choice([0, 1])]))
    if rng.random() < 0.10:
        out += tlv(0x7FFE, b"vendor")  # unknown tag, must be ignored
    return out


def main() -> int:
    ap = argparse.ArgumentParser()
    ap.add_argument("--frames", type=int, default=20000)
    ap.add_argument("--sessions", type=int, default=8)
    ap.add_argument("--out", default="input.jsonl")
    ap.add_argument("--fragment-rate", type=float, default=0.25)
    ap.add_argument("--corrupt-rate", type=float, default=0.04)
    ap.add_argument("--reorder-rate", type=float, default=0.05)
    ap.add_argument("--seed", type=int, default=7)
    ap.add_argument("--truth", default=None, help="write expected timeline here")
    args = ap.parse_args()

    rng = random.Random(args.seed)
    sessions = [f"sub-{i:04d}" for i in range(args.sessions)]
    seqs = {s: 0 for s in sessions}

    pending: list[tuple] = []  # (session, ts, frame_bytes)
    lines: list[str] = []
    truth: list[dict] = []
    corrupt = 0

    per_session = max(1, args.frames // args.sessions)
    total = per_session * args.sessions

    for i in range(total):
        sid = sessions[i % args.sessions]
        seq = seqs[sid]
        seqs[sid] += 1
        body = make_body(rng, seq)
        ts = 1757000000000 + i * 17

        if rng.random() < args.fragment_rate:
            # A fragmented frame is one logical frame split across several lines;
            # build_fragments gives every piece the same frame-level CRC.
            nfrag = rng.randint(2, 4)
            for fb in build_fragments(seq, body, nfrag):
                pending.append((sid, ts, fb))
        else:
            pending.append((sid, ts, build_frame(seq, body)))

        # Local reordering only: a real jittery link swaps adjacent-ish frames,
        # it does not shuffle the whole file.  A full shuffle would move frames
        # arbitrarily far from their slot, which is unrecoverable data loss
        # rather than reordering.
        if rng.random() < args.reorder_rate and len(pending) > 1:
            j = max(0, len(pending) - 1 - rng.randrange(3))
            pending.append(pending.pop(j))

        while pending and rng.random() < 0.5:
            s, t, fb = pending.pop(0)
            r = rng.random()
            if r < args.corrupt_rate:
                kind = rng.choice(["badjson", "badb64", "trunc", "crc", "garbage"])
                corrupt += 1
                if kind == "badjson":
                    lines.append('{"session_id": "%s", "ts": %d, "payload": ' % (s, t))
                elif kind == "badb64":
                    lines.append(json.dumps({"session_id": s, "ts": t, "payload": "!!!!not-b64!!!"}))
                elif kind == "trunc":
                    lines.append(json.dumps({"session_id": s, "ts": t,
                                             "payload": base64.b64encode(fb[:8]).decode()}))
                elif kind == "crc":
                    bad = bytearray(fb)
                    bad[-1] ^= 0xFF
                    lines.append(json.dumps({"session_id": s, "ts": t,
                                             "payload": base64.b64encode(bytes(bad)).decode()}))
                else:
                    lines.append("<<< garbage line >>>")
            else:
                lines.append(json.dumps({"session_id": s, "ts": t,
                                         "payload": base64.b64encode(fb).decode()}))

    for s, t, fb in pending:
        lines.append(json.dumps({"session_id": s, "ts": t,
                                 "payload": base64.b64encode(fb).decode()}))

    with open(args.out, "w", encoding="utf-8") as fh:
        fh.write("\n".join(lines) + "\n")

    print(f"wrote {args.out}: {len(lines)} lines, {args.sessions} sessions, "
          f"{corrupt} corrupted")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
