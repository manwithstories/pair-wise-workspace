"""SRT and WebVTT parsing with byte-level provenance.

The parser never silently drops a record. Bad time ranges, truncated cue
blocks and overlapping cues each produce an
:class:`relaymedia.errors.ErrorRecord` carrying the record's byte offset and
1-based line number; an isolatable bad block is skipped while later blocks
keep parsing. Cue text and the raw timing strings are retained verbatim, and
the original timeline offset (no normalisation to zero) is preserved on every
cue.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

from .clock import parse_clock_time
from .errors import ErrorRecord, InputError, Stage, first_offset

_ARROW = " --> "


@dataclass(frozen=True, slots=True)
class SubtitleCue:
    index: int
    start: Fraction
    end: Fraction
    text: str
    raw_start: str
    raw_end: str
    source: str
    start_offset_bytes: int
    end_offset_bytes: int
    start_line: int
    end_line: int


@dataclass(slots=True)
class SubtitleTrack:
    source: str
    fmt: str
    cues: list[SubtitleCue] = field(default_factory=list)
    errors: list[ErrorRecord] = field(default_factory=list)

    @property
    def bad_record_count(self) -> int:
        return len(self.errors)

    @property
    def first_error_offset_bytes(self) -> int:
        return first_offset(self.errors)


def _line_view(data: bytes) -> list[tuple[int, int, int, bytes]]:
    """Split bytes into (start_offset, end_offset, line_no, line_without_eol)."""
    view: list[tuple[int, int, int, bytes]] = []
    offset = 0
    line_no = 0
    for raw in data.split(b"\n"):
        line_no += 1
        if raw.endswith(b"\r"):
            raw = raw[:-1]
        view.append((offset, offset + len(raw), line_no, raw))
        offset += len(raw) + 1  # +1 for the consumed b"\n"
    return view


def _decode(raw: bytes) -> str:
    try:
        return raw.decode("utf-8")
    except UnicodeDecodeError:
        # Keep parsing; replacement characters never feed a timing line check.
        return raw.decode("utf-8", errors="replace")


def _parse_timing(line: str, sep: str) -> tuple[Fraction, Fraction, str, str]:
    if _ARROW not in line:
        raise InputError(f"timing line missing ' --> ': {line!r}", stage=Stage.SUBTITLE)
    left, right = line.split(_ARROW, 1)
    raw_start = left.strip()
    # VTT allows cue settings after the end time; the first token is the clock.
    raw_end = right.strip().split()[0]
    start = parse_clock_time(raw_start, sep=sep)
    end = parse_clock_time(raw_end, sep=sep)
    if end < start:
        raise InputError(
            f"cue ends before it starts: {raw_start} -> {raw_end}",
            stage=Stage.SUBTITLE,
        )
    return start, end, raw_start, raw_end


def _blocks(view: list[tuple[int, int, int, bytes]]) -> list[list[tuple[int, int, int, bytes]]]:
    blocks: list[list[tuple[int, int, int, bytes]]] = []
    current: list[tuple[int, int, int, bytes]] = []
    for item in view:
        if item[3].strip() == b"":
            if current:
                blocks.append(current)
                current = []
            continue
        current.append(item)
    if current:
        blocks.append(current)
    return blocks


def _fail(track: SubtitleTrack, message: str, *, offset: int, line: int) -> None:
    track.errors.append(
        ErrorRecord(
            stage=Stage.SUBTITLE.value,
            kind="InputError",
            message=message,
            source=track.source,
            offset_bytes=offset,
            line=line,
        )
    )


def _parse_blocks(track: SubtitleTrack, blocks: list[list[tuple[int, int, int, bytes]]], sep: str) -> None:
    next_index = 1
    for block in blocks:
        b_start, _, b_line, _ = block[0]
        b_end = block[-1][1]
        lines = [_decode(item[3]) for item in block]
        try:
            if lines[0].lstrip().startswith(("NOTE", "STYLE", "REGION")):
                continue  # VTT comment/metadata blocks are skipped, not errors.
            if _ARROW in lines[0]:
                timing_line, text_lines = lines[0], lines[1:]
            elif len(lines) >= 2 and _ARROW in lines[1]:
                timing_line, text_lines = lines[1], lines[2:]
            else:
                raise InputError("truncated cue block: no timing line", stage=Stage.SUBTITLE)
            start, end, raw_start, raw_end = _parse_timing(timing_line, sep)
        except InputError as exc:
            _fail(track, exc.message, offset=b_start, line=b_line)
            continue
        if not any(t.strip() for t in text_lines):
            _fail(track, "truncated cue block: no text lines", offset=b_start, line=b_line)
            continue
        track.cues.append(
            SubtitleCue(
                index=next_index,
                start=start,
                end=end,
                text="\n".join(text_lines),
                raw_start=raw_start,
                raw_end=raw_end,
                source=track.source,
                start_offset_bytes=b_start,
                end_offset_bytes=b_end,
                start_line=b_line,
                end_line=block[-1][2],
            )
        )
        next_index += 1
    for prev, cue in zip(track.cues, track.cues[1:]):
        if cue.start < prev.end:
            _fail(
                track,
                f"overlap: cue {cue.index} starts at {cue.start} before cue {prev.index} ends at {prev.end}",
                offset=cue.start_offset_bytes,
                line=cue.start_line,
            )


def parse_subtitles(path: str | Path, fmt: str | None = None) -> SubtitleTrack:
    """Parse an ``.srt`` or ``.vtt`` file; ``fmt`` overrides extension sniffing."""
    path = Path(path)
    data = path.read_bytes()
    if fmt is None:
        fmt = "vtt" if data.lstrip(b"\xef\xbb\xbf").startswith(b"WEBVTT") else (
            "vtt" if path.suffix.lower() == ".vtt" else "srt"
        )
    track = SubtitleTrack(source=str(path), fmt=fmt)
    view = _line_view(data)
    if fmt == "vtt":
        first = next((v for v in view if v[3].strip()), None)
        if first is None or not _decode(first[3]).lstrip("﻿").startswith("WEBVTT"):
            _fail(track, "WebVTT file missing WEBVTT header", offset=0, line=1)
        blocks = [b for b in _blocks(view) if not _decode(b[0][3]).lstrip().startswith("WEBVTT")]
        _parse_blocks(track, blocks, sep=".")
    else:
        _parse_blocks(track, _blocks(view), sep=",")
    return track
