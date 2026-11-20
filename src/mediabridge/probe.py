"""Streaming container, track and packet inspection.

``probe`` never reads the whole file into memory.  It opens the container
with PyAV, snapshots the streams, then walks packets one at a time, yielding
:class:`PacketInfo` records the rest of the pipeline consumes.  Failures at
container level (missing index, unsupported codec, zero packets) surface as
typed :class:`~mediabridge.errors.StructuralError` subclasses; a damaged
packet merely marks itself ``corrupt`` and the walk continues.

The packet walk is the single source of truth for packet numbering: both
``repair`` and ``verify`` derive the same global and per-track indices from
it, which is what makes ``first_error_packet`` reproducible to the packet.
"""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Callable, Iterator

import av

from .errors import (
    StructuralError,
    TruncatedInputError,
    UnsupportedCodecError,
)
from .timeline import PacketTimestamps, TrackSpec, parse_time_base

#: Container-level codecs this pipeline can copy without transcoding.
SUPPORTED_VIDEO = {"h264"}
SUPPORTED_AUDIO = {"aac"}

#: Read buffer used for hashing and chunk fingerprints (bytes).
HASH_CHUNK = 1 << 20


@dataclass
class PacketInfo:
    """One demuxed packet, enough to repair and index without re-decoding."""

    global_index: int
    track_index: int
    stream_index: int
    dts: int | None
    pts: int | None
    duration: int | None
    size: int
    pos: int | None
    is_keyframe: bool
    is_corrupt: bool
    time_base: Fraction

    def as_timestamps(self) -> PacketTimestamps:
        return PacketTimestamps(
            track_index=self.track_index,
            stream_index=self.stream_index,
            packet_index=0,
            global_index=self.global_index,
            dts=self.dts,
            pts=self.pts,
            time_base=self.time_base,
            size=self.size,
            pos=self.pos,
            is_keyframe=self.is_keyframe,
        )


@dataclass
class ProbeResult:
    path: Path
    size: int
    mtime_ns: int
    format_name: str
    tracks: list[TrackSpec]
    duration: Fraction | None = None
    packet_count: int = 0
    first_packets: list[PacketInfo] = field(default_factory=list)
    corrupted_packets: list[int] = field(default_factory=list)

    def as_dict(self) -> dict:
        return {
            "path": str(self.path),
            "size": self.size,
            "mtime_ns": self.mtime_ns,
            "format": self.format_name,
            "duration_seconds": float(self.duration) if self.duration is not None else None,
            "tracks": [track.as_dict() for track in self.tracks],
            "packet_count": self.packet_count,
            "corrupted_packets": list(self.corrupted_packets),
        }


def file_fingerprint(path: Path) -> dict:
    """Size, mtime and a fast head/tail digest used for resume validation.

    The digest covers the first and last 1 MiB plus the file size so it is
    cheap on large archives yet changes whenever the container index or the
    tail of the file changes - the places repairable damage lives.
    """
    stat = os.stat(path)
    digest = hashlib.sha256()
    digest.update(str(stat.st_size).encode())
    with open(path, "rb") as handle:
        head = handle.read(HASH_CHUNK)
        digest.update(head)
        if stat.st_size > HASH_CHUNK:
            handle.seek(max(HASH_CHUNK, stat.st_size - HASH_CHUNK))
            digest.update(handle.read(HASH_CHUNK))
    return {
        "path": str(Path(path).resolve()),
        "size": stat.st_size,
        "mtime_ns": stat.st_mtime_ns,
        "digest": digest.hexdigest(),
    }


def _track_specs(container: av.container.InputContainer) -> list[TrackSpec]:
    specs: list[TrackSpec] = []
    track_index = 0
    for stream in container.streams:
        if stream.type not in ("video", "audio"):
            continue
        codec = stream.codec_context.name
        if codec not in (SUPPORTED_VIDEO if stream.type == "video" else SUPPORTED_AUDIO):
            raise UnsupportedCodecError(
                f"unsupported {stream.type} codec {codec!r}; "
                f"supported: {sorted(SUPPORTED_VIDEO if stream.type == 'video' else SUPPORTED_AUDIO)}",
                stream_index=stream.index,
                context={"codec": codec, "type": stream.type},
            )
        sample_rate = None
        channels = None
        if stream.type == "audio":
            sample_rate = stream.codec_context.sample_rate
            channels = stream.codec_context.channels
        rate = None
        if stream.type == "video":
            try:
                rate = Fraction(stream.average_rate)
            except (TypeError, ZeroDivisionError):
                rate = None
        specs.append(TrackSpec(
            stream_index=stream.index,
            track_index=track_index,
            kind=stream.type,
            codec=codec,
            time_base=parse_time_base(stream.time_base),
            start_time=stream.start_time,
            average_rate=rate,
            sample_rate=sample_rate,
            channels=channels,
        ))
        track_index += 1
    return specs


def iter_packets(container: av.container.InputContainer,
                 tracks: list[TrackSpec]) -> Iterator[tuple[PacketInfo | None, object | None]]:
    """Yield ``(info, packet)`` in demux order, tolerating demux failures.

    ``PacketInfo`` is the stable, serialisable description; ``packet`` is the
    live ``av.Packet`` (``None`` when the demuxer itself failed).  Both
    ``probe`` and ``repair``/``verify`` use this iterator so packet numbering
    is identical across every stage.
    """
    by_stream = {spec.stream_index: spec for spec in tracks}
    time_bases = {spec.stream_index: spec.time_base for spec in tracks}
    global_index = 0
    # Keep ONE demux generator: calling ``container.demux()`` per iteration
    # would restart the demuxer and loop forever.
    demux = container.demux()
    while True:
        try:
            packet = next(demux)
        except StopIteration:
            break
        except av.AVError:
            yield PacketInfo(
                global_index=global_index,
                track_index=-1,
                stream_index=-1,
                dts=None, pts=None, duration=None,
                size=0, pos=None, is_keyframe=False, is_corrupt=True,
                time_base=Fraction(1, 1),
            ), None
            global_index += 1
            continue
        if packet.dts is None and packet.pts is None and packet.size == 0:
            continue
        spec = by_stream.get(packet.stream.index)
        if spec is None:
            continue
        info = PacketInfo(
            global_index=global_index,
            track_index=spec.track_index,
            stream_index=packet.stream.index,
            dts=packet.dts,
            pts=packet.pts,
            duration=packet.duration,
            size=packet.size,
            pos=packet.pos,
            is_keyframe=bool(packet.is_keyframe),
            is_corrupt=bool(getattr(packet, "is_corrupt", False)),
            time_base=time_bases[spec.stream_index],
        )
        yield info, packet
        global_index += 1


def walk_packets(container: av.container.InputContainer,
                 tracks: list[TrackSpec],
                 on_packet: Callable[[PacketInfo], None] | None = None,
                 *,
                 limit: int | None = None) -> Iterator[PacketInfo]:
    """Yield packet descriptions in demux order, tolerating damaged packets."""
    for index, (info, _packet) in enumerate(iter_packets(container, tracks)):
        yield info
        if on_packet:
            on_packet(info)
        if limit is not None and index + 1 >= limit:
            return


def open_container(path: Path) -> av.container.InputContainer:
    try:
        return av.open(str(path), mode="r")
    except av.AVError as exc:
        raise StructuralError(f"cannot open container {path}: {exc}") from exc


def probe(path: Path, *, sample_limit: int = 4) -> ProbeResult:
    """Open a file and describe it without reading all packets."""
    path = Path(path)
    if not path.exists():
        raise StructuralError(f"input does not exist: {path}")
    if path.is_dir():
        raise StructuralError(f"input is a directory: {path}")
    stat = os.stat(path)
    if stat.st_size == 0:
        raise TruncatedInputError("input file is empty", context={"path": str(path)})
    fingerprint = file_fingerprint(path)
    container = open_container(path)
    try:
        tracks = _track_specs(container)
        if not tracks:
            raise StructuralError("container has no audio or video streams", context={"path": str(path)})
        duration = None
        if container.duration is not None:
            duration = Fraction(container.duration, av.time_base)
        sample: list[PacketInfo] = []
        corrupted: list[int] = []
        count = 0
        for info in walk_packets(container, tracks):
            count += 1
            if len(sample) < sample_limit:
                sample.append(info)
            if info.is_corrupt:
                corrupted.append(info.global_index)
        if count == 0:
            raise TruncatedInputError(
                "container exposes streams but no readable packets",
                context={"path": str(path), "tracks": [t.kind for t in tracks]},
            )
        return ProbeResult(
            path=path,
            size=fingerprint["size"],
            mtime_ns=fingerprint["mtime_ns"],
            format_name=container.format.name if container.format else "",
            tracks=tracks,
            duration=duration,
            packet_count=count,
            first_packets=sample,
            corrupted_packets=corrupted,
        )
    finally:
        container.close()
