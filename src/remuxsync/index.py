"""Keyframe index validation, quarantine, rebuild and seek verification."""

from __future__ import annotations

import json
from dataclasses import dataclass, field

from .errors import ErrorLog, Recovery, Stage


@dataclass
class IndexEntry:
    frame: int
    offset: int  # byte offset in the stream this index describes
    ts_ticks: int = 0  # unified-timeline ticks (filled during rebuild)

    def to_dict(self) -> dict:
        return {"frame": self.frame, "offset": self.offset, "ts_ticks": self.ts_ticks}


@dataclass
class RandomAccessIndex:
    entries: list[IndexEntry] = field(default_factory=list)

    def seek(self, frame: int) -> IndexEntry | None:
        """Largest indexed frame <= target, else None."""
        best: IndexEntry | None = None
        for e in self.entries:
            if e.frame <= frame and (best is None or e.frame > best.frame):
                best = e
        return best

    def to_json(self, track: str) -> str:
        return json.dumps(
            {"track": track, "entries": [e.to_dict() for e in self.entries]},
            indent=2,
        )


def load_index(path: str) -> list[IndexEntry]:
    with open(path, "r", encoding="utf-8") as f:
        raw = json.load(f)
    entries = raw.get("entries", raw) if isinstance(raw, dict) else raw
    out = []
    for e in entries:
        out.append(IndexEntry(frame=int(e["frame"]), offset=int(e["offset"]),
                              ts_ticks=int(e.get("ts_ticks", 0))))
    return out


def validate_index(
    entries: list[IndexEntry],
    stream_size: int,
    keyframe_offsets: set[int],
    log: ErrorLog,
    track: str,
) -> list[IndexEntry]:
    """Quarantine corrupt entries; return the verifiable ones.

    Checks: out-of-bounds offsets, duplicates, misordering and offsets that
    do not land on an actual keyframe unit boundary.
    """
    valid: list[IndexEntry] = []
    seen_frames: set[int] = set()
    seen_offsets: set[int] = set()
    last_offset = -1
    for e in entries:
        reason = None
        if e.offset < 0 or e.offset >= max(stream_size, 1):
            reason = f"offset {e.offset} out of bounds (stream size {stream_size})"
        elif e.frame in seen_frames or e.offset in seen_offsets:
            reason = f"duplicate entry frame={e.frame} offset={e.offset}"
        elif e.offset < last_offset:
            reason = f"misordered offset {e.offset} after {last_offset}"
        elif e.offset not in keyframe_offsets:
            reason = f"offset {e.offset} does not match any keyframe boundary"
        if reason is None:
            valid.append(e)
            seen_frames.add(e.frame)
            seen_offsets.add(e.offset)
            last_offset = e.offset
        else:
            log.add(Stage.INDEX, track, f"quarantined index entry: {reason}",
                    byte_offset=max(e.offset, 0), recovery=Recovery.QUARANTINED)
    return valid


def rebuild_index(
    keyframe_units: list,
    frame_of_unit: dict[int, int],
    ts_of_unit: dict[int, int],
) -> RandomAccessIndex:
    """Rebuild a random-access index from the verified keyframe units."""
    entries = []
    for u in keyframe_units:
        frame = frame_of_unit.get(u.offset)
        if frame is None:
            continue
        entries.append(IndexEntry(frame=frame, offset=u.offset,
                                  ts_ticks=ts_of_unit.get(u.offset, 0)))
    entries.sort(key=lambda e: e.frame)
    return RandomAccessIndex(entries)


def verify_seeks(
    index: RandomAccessIndex,
    keyframe_offsets: set[int],
    log: ErrorLog,
    track: str,
) -> int:
    """Seek-verify first, middle and last indexed frames. Returns failures."""
    if not index.entries:
        log.add(Stage.VALIDATE, track, "rebuilt index is empty",
                recovery=Recovery.UNRECOVERABLE)
        return 1
    targets = sorted({
        index.entries[0].frame,
        index.entries[len(index.entries) // 2].frame,
        index.entries[-1].frame,
    })
    failures = 0
    for frame in targets:
        hit = index.seek(frame)
        if hit is None or hit.offset not in keyframe_offsets:
            off = hit.offset if hit else None
            log.add(Stage.VALIDATE, track,
                    f"seek verification failed for frame {frame}",
                    byte_offset=off, recovery=Recovery.UNRECOVERABLE)
            failures += 1
    return failures
