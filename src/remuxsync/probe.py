"""Codec-level probing of recovered elementary streams via PyAV.

The boundary scanner understands framing; this module adds an independent
codec-level sanity check using PyAV's ``CodecContext.parse``. Synthetic sample
streams carry placeholder payloads that cannot be fully decoded, so probe
results are advisory metadata (never fatal) — real captured streams would
yield parsed/decoded packets here.
"""

from __future__ import annotations

import av

_CODEC_NAMES = {"h264": "h264", "h265": "hevc", "av1": "av1"}
PROBE_CHUNK = 1 << 16
PROBE_LIMIT = 1 << 20  # probe at most the first 1 MiB, in bounded chunks


def probe_stream(path: str, codec: str) -> dict:
    """Return codec probe metadata; never raises on undecodable samples."""
    name = _CODEC_NAMES.get(codec)
    base = {"codec": codec, "parsed_packets": 0, "decodable": False,
            "probe_bytes": 0, "available": False}
    if name is None:
        return base
    try:
        ctx = av.codec.CodecContext.create(name, "r")
    except (av.FFmpegError, ValueError):
        return base  # codec unavailable on this FFmpeg build

    parsed = 0
    consumed = 0
    try:
        with open(path, "rb") as f:
            while consumed < PROBE_LIMIT:
                chunk = f.read(PROBE_CHUNK)
                if not chunk:
                    break
                consumed += len(chunk)
                parsed += len(ctx.parse(chunk))
    except av.FFmpegError:
        return {**base, "parsed_packets": parsed, "probe_bytes": consumed,
                "available": True, "malformed": True}
    return {
        "codec": codec,
        "parsed_packets": parsed,
        "decodable": parsed > 0,
        "probe_bytes": consumed,
        "available": True,
    }
