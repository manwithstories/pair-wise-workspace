"""SRT / WebVTT parsing with exact source byte offsets and recoverable errors.

Illegal timestamps, truncated lines and overlapping cues are recorded in the
error log (with byte offset and record index); parsing continues with the next
recoverable record.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from pathlib import Path

from .errors import ErrorLog

_SRT_TIMING = re.compile(
    r"^(\d{1,2}):(\d{2}):(\d{2}),(\d{3})\s*-->\s*(\d{1,2}):(\d{2}):(\d{2}),(\d{3})")
_VTT_TIMING = re.compile(
    r"^(?:(\d{1,2}):)?(\d{1,2}):(\d{2})\.(\d{3})\s*-->\s*(?:(\d{1,2}):)?(\d{1,2}):(\d{2})\.(\d{3})")


@dataclass
class Cue:
    index: int          # record ordinal within the file (0-based)
    start_ms: int
    end_ms: int
    text: str
    source: str
    source_offset: int  # byte offset of the first byte of this cue's block


def _to_ms(h: str, m: str, s: str, ms: str) -> int | None:
    hh, mm, ss = int(h or 0), int(m), int(s)
    if mm >= 60 or ss >= 60:
        return None
    return ((hh * 60 + mm) * 60 + ss) * 1000 + int(ms)


def _split_blocks(data: bytes) -> list[tuple[int, list[tuple[int, bytes]]]]:
    """Split into blank-line-separated blocks.

    Returns [(block_byte_offset, [(line_byte_offset, raw_line), ...]), ...].
    """
    blocks: list[tuple[int, list[tuple[int, bytes]]]] = []
    current: list[tuple[int, bytes]] = []
    pos = 0
    for raw in data.split(b"\n"):
        line_off = pos
        pos += len(raw) + 1
        line = raw.rstrip(b"\r")
        if line.strip() == b"":
            if current:
                blocks.append((current[0][0], current))
                current = []
        else:
            current.append((line_off, line))
    if current:
        blocks.append((current[0][0], current))
    return blocks


def _decode(line: bytes) -> str:
        return line.decode("utf-8", errors="replace")


def _parse_timing(line: str, pattern: re.Pattern) -> tuple[int, int] | None:
    m = pattern.match(line.strip())
    if not m:
        return None
    g = m.groups()
    half = len(g) // 2
    start = _to_ms(g[0] or "0", g[1], g[2], g[3])
    end = _to_ms(g[half] or "0", g[half + 1], g[half + 2], g[half + 3])
    if start is None or end is None or end <= start:
        return None
    return start, end


def parse_srt(path: str | Path, log: ErrorLog) -> list[Cue]:
    src = str(path)
    data = Path(path).read_bytes()
    cues: list[Cue] = []
    for rec_idx, (blk_off, lines) in enumerate(_split_blocks(data)):
        texts = [_decode(l) for _, l in lines]
        if len(lines) < 2:
            log.add("truncated_record",
                    f"SRT block #{rec_idx} has {len(lines)} line(s); expected index + timing",
                    src, blk_off, rec_idx)
            continue
        if not texts[0].strip().isdigit():
            log.add("truncated_record",
                    f"SRT block #{rec_idx} missing numeric index line",
                    src, lines[0][0], rec_idx)
            continue
        timing_off = lines[1][0]
        if "-->" not in texts[1]:
            log.add("truncated_record",
                    f"SRT block #{rec_idx} timing line is cut off",
                    src, timing_off, rec_idx)
            continue
        parsed = _parse_timing(texts[1], _SRT_TIMING)
        if parsed is None:
            log.add("invalid_time",
                    f"SRT block #{rec_idx} has illegal or reversed timing: {texts[1].strip()!r}",
                    src, timing_off, rec_idx)
            continue
        cues.append(Cue(rec_idx, parsed[0], parsed[1],
                        " ".join(t.strip() for t in texts[2:]), src, blk_off))
    _check_overlaps(cues, log)
    return cues


def parse_vtt(path: str | Path, log: ErrorLog) -> list[Cue]:
    src = str(path)
    data = Path(path).read_bytes()
    blocks = _split_blocks(data)
    cues: list[Cue] = []
    rec_idx = 0
    start_i = 0
    if blocks and _decode(blocks[0][1][0][1]).lstrip("﻿").startswith("WEBVTT"):
        start_i = 1
    else:
        log.add("invalid_header", "WebVTT file missing WEBVTT header", src, 0, -1)
    for blk_off, lines in blocks[start_i:]:
        texts = [_decode(l) for _, l in lines]
        if texts[0].startswith("NOTE"):
            continue
        # optional cue identifier line, then timing line
        ti = 0
        if "-->" not in texts[0] and len(texts) > 1:
            ti = 1
        if ti >= len(texts) or "-->" not in texts[ti]:
            log.add("truncated_record",
                    f"VTT cue #{rec_idx} missing timing line", src, blk_off, rec_idx)
            rec_idx += 1
            continue
        parsed = _parse_timing(texts[ti], _VTT_TIMING)
        if parsed is None:
            log.add("invalid_time",
                    f"VTT cue #{rec_idx} has illegal or reversed timing: {texts[ti].strip()!r}",
                    src, lines[ti][0], rec_idx)
            rec_idx += 1
            continue
        cues.append(Cue(rec_idx, parsed[0], parsed[1],
                        " ".join(t.strip() for t in texts[ti + 1:]), src, blk_off))
        rec_idx += 1
    _check_overlaps(cues, log)
    return cues


def _check_overlaps(cues: list[Cue], log: ErrorLog) -> None:
    for prev, cur in zip(cues, cues[1:]):
        if cur.start_ms < prev.end_ms:
            log.add("overlap",
                    f"cue #{cur.index} starts at {cur.start_ms}ms before cue #{prev.index} "
                    f"ends at {prev.end_ms}ms",
                    cur.source, cur.source_offset, cur.index)


def parse_subtitles(path: str | Path, log: ErrorLog) -> list[Cue]:
    """Dispatch on content: WebVTT if it starts with WEBVTT, else SRT."""
    head = Path(path).read_bytes()[:16].lstrip(b"\xef\xbb\xbf")
    if head.startswith(b"WEBVTT"):
        return parse_vtt(path, log)
    return parse_srt(path, log)
