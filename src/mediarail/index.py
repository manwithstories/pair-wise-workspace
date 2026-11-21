"""Rebuild a random-access index over a repaired output file and verify it.

The index is built by re-scanning the finished output so every entry carries
the true byte offset of a seekable packet.  Verification decodes from each
entry and confirms the decoded frame lands on the indexed presentation
time; a failed entry refuses publication of the output.
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av

from .errors import ErrorCode, MediaError, Severity

INDEX_SUFFIX = ".midx"
INDEX_VERSION = 1


@dataclass
class IndexEntry:
    track: str
    stream_index: int
    pts_seconds: float
    byte_offset: int
    size: int

    def to_dict(self) -> dict:
        return {
            "track": self.track,
            "stream_index": self.stream_index,
            "pts": self.pts_seconds,
            "byte_offset": self.byte_offset,
            "size": self.size,
        }


@dataclass
class MediaIndex:
    entries: list[IndexEntry] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "version": INDEX_VERSION,
            "entries": [e.to_dict() for e in self.entries],
        }

    @classmethod
    def from_dict(cls, data: dict) -> "MediaIndex":
        if data.get("version") != INDEX_VERSION:
            raise ValueError(f"unsupported index version {data.get('version')}")
        return cls(
            entries=[
                IndexEntry(
                    track=e["track"],
                    stream_index=e["stream_index"],
                    pts_seconds=float(e["pts"]),
                    byte_offset=int(e["byte_offset"]),
                    size=int(e["size"]),
                )
                for e in data["entries"]
            ]
        )


def build_index(output_path: str | Path) -> MediaIndex:
    """Collect every random-access point of the finished output file.

    Video keyframes and every audio packet (audio is independently
    decodable) become index entries, with byte offsets taken from the
    muxed file itself.
    """
    index = MediaIndex()
    container = av.open(str(output_path))
    try:
        kinds = {s.index: s.type for s in container.streams}
        for packet in container.demux():
            kind = kinds.get(packet.stream_index)
            if kind == "video" and not packet.is_keyframe:
                continue
            if kind not in ("video", "audio"):
                continue
            if packet.pts is None or packet.pos is None:
                continue
            stream = container.streams[packet.stream_index]
            pts_seconds = float(Fraction(packet.pts) * Fraction(stream.time_base))
            index.entries.append(
                IndexEntry(
                    track=kind,
                    stream_index=packet.stream_index,
                    pts_seconds=pts_seconds,
                    byte_offset=packet.pos,
                    size=packet.size or 0,
                )
            )
    finally:
        container.close()
    index.entries.sort(key=lambda e: (e.stream_index, e.pts_seconds))
    return index


def write_index(index: MediaIndex, path: str | Path) -> None:
    Path(path).write_text(json.dumps(index.to_dict(), indent=1))


def read_index(path: str | Path) -> MediaIndex:
    return MediaIndex.from_dict(json.loads(Path(path).read_text()))


def verify_index(
    media_path: str | Path, index: MediaIndex, tolerance: float = 0.05
) -> list[MediaError]:
    """Decode from each entry and confirm the target presentation time.

    Returns one :class:`MediaError` per entry that cannot be decoded to its
    indexed presentation time; an empty list means the index is publishable.
    """
    errors: list[MediaError] = []
    container = av.open(str(media_path))
    try:
        for entry in index.entries:
            stream = container.streams[entry.stream_index]
            target = entry.pts_seconds
            try:
                container.seek(
                    int(target / float(stream.time_base)),
                    stream=stream,
                    backward=True,
                )
                hit = False
                for frame in container.decode(stream):
                    frame_time = float(frame.pts * stream.time_base) if frame.pts is not None else None
                    if frame_time is None:
                        continue
                    if frame_time >= target - tolerance:
                        if abs(frame_time - target) <= tolerance:
                            hit = True
                        break
                if not hit:
                    errors.append(
                        MediaError(
                            code=ErrorCode.INDEX_VERIFY_FAILED,
                            message=(
                                f"no decodable frame at {target:.3f}s on "
                                f"{entry.track} stream"
                            ),
                            severity=Severity.FATAL,
                            track=entry.track,
                            byte_offset=entry.byte_offset,
                        )
                    )
            except av.error.FFmpegError as exc:
                errors.append(
                    MediaError(
                        code=ErrorCode.INDEX_VERIFY_FAILED,
                        message=f"decode from index entry failed: {exc}",
                        severity=Severity.FATAL,
                        track=entry.track,
                        byte_offset=entry.byte_offset,
                    )
                )
    finally:
        container.close()
    return errors
