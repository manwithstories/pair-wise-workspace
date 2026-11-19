"""Lossless remux: write kept packets byte-for-byte and rebuild the index.

Ordering is a dependency, not a preference:

1. The track graph fixes output slots (video first, then audio, then the rest).
2. Every output stream is created with the *source* codec parameters
   (``add_stream(template=...)``), so extradata and time base are inherited.
3. Packets are written in graph order; each packet's payload bytes are copied
   unchanged and only its stream reference is retargeted.
4. The container index is finalised by the muxer on close, i.e. strictly after
   every packet has been written.

A shared presentation origin is subtracted from *all* streams at once.  Shifting
streams independently is what makes naive remuxers drift: subtracting the same
instant from every stream preserves relative A/V timing exactly.

Nothing here decodes or encodes.  It is also the place that refuses to write
when a codec combination cannot be copied losslessly (see
:func:`check_lossless_support` in :mod:`bcastqc.demux`).
"""

from __future__ import annotations

import hashlib
import os
import shutil
import tempfile
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av

from .demux import AssetProbe, RawPacket, check_lossless_support, format_guess
from .errors import FatalError, LosslessCopyRefusedError, MediaLocation
from .trackmap import TrackGraph, TrackNode


@dataclass(slots=True)
class OutputTrackStats:
    output: int
    source: int
    packets: int = 0
    keyframes: int = 0
    bytes: int = 0
    digest: str = ""

    def to_dict(self) -> dict[str, object]:
        return {
            "output": self.output,
            "source": self.source,
            "packets": self.packets,
            "keyframes": self.keyframes,
            "bytes": self.bytes,
            "packet_digest": self.digest,
        }


@dataclass(slots=True)
class RemuxResult:
    output_path: Path
    bytes_written: int
    packets_written: int
    per_track: list[OutputTrackStats]
    origin: Fraction
    origin_spread: Fraction = Fraction(0)
    shifts: dict[int, int] = field(default_factory=dict)
    stream_map: dict[int, int] = field(default_factory=dict)

    def to_dict(self) -> dict[str, object]:
        return {
            "output_path": str(self.output_path),
            "bytes_written": self.bytes_written,
            "packets_written": self.packets_written,
            "origin": round(float(self.origin), 9),
            "origin_spread_ms": round(float(self.origin_spread) * 1000.0, 6),
            "stream_map": {str(k): v for k, v in sorted(self.stream_map.items())},
            "shifts_ticks": {str(k): v for k, v in sorted(self.shifts.items())},
            "per_track": [t.to_dict() for t in self.per_track],
        }


class RemuxError(FatalError):
    code = "remux_error"


def _packet_times(packet: RawPacket, shift: int, time_base: Fraction) -> tuple[int | None, int | None]:
    """Apply an integer tick shift in the packet's own time base."""
    del time_base
    pts = None if packet.pts is None else packet.pts - shift
    dts = None if packet.dts is None else packet.dts - shift
    return pts, dts


def _origin_ticks(
    graph: TrackGraph,
    packets_by_stream: dict[int, list[RawPacket]],
    declared: dict[int, Fraction] | None = None,
) -> tuple[Fraction, dict[int, int], Fraction]:
    """Pick a shared origin (in ticks) and the per-stream integer shift.

    The origin is the *declared* presentation start of the earliest stream
    (the container's own idea of where the programme begins), not the minimum
    raw packet timestamp.  Audio tracks routinely carry a priming packet with
    a negative timestamp (AAC encoder delay, 21 ms here); using it as the
    origin would push the whole programme later on the timeline without any
    visible sync error, because both streams move together.  Falling back to
    raw timestamps only happens when the container declares no start time.

    Every stream is shifted by the *same presentation instant*; the tick value
    differs per stream because each has its own time base.
    """
    declared = declared or {}
    origins: list[tuple[Fraction, int, str]] = []
    for node in graph.ordered():
        index = node.source.index
        start = declared.get(index)
        if start is None:
            stream_packets = packets_by_stream.get(index, [])
            first = next((p for p in stream_packets if p.pts is not None), None)
            start = None if first is None else Fraction(first.pts) * first.time_base
        if start is not None:
            origins.append((start, index, node.source.type))
    if not origins:
        return Fraction(0), {node.source.index: 0 for node in graph.nodes}, Fraction(0)
    origin_seconds: Fraction | None = None
    if declared:
        # Anchor on the declared start so encoder priming does not displace the
        # programme.  Prefer picture, then audio, then anything else: an
        # operator judges a master against its first frame, and a tiny audio
        # priming lead must not drag the timeline.
        for kind in ("video", "audio"):
            starts = [start for start, _, stream_kind in origins if stream_kind == kind]
            if starts:
                origin_seconds = min(starts)
                break
    if origin_seconds is None:
        # No declared start anywhere: fall back to the earliest first packet,
        # which is always the safe choice because nothing can then go negative.
        origin_seconds = min(start for start, _, _ in origins)

    # A single early stream (say audio 1 s before picture) must not push the
    # other stream's packets to deeply negative timestamps: the muxer then
    # silently drops them, losing real media behind a perfect-looking sync
    # score.  Lower the origin so no stream's first packet is more than one
    # frame negative -- the same small negative offset an AAC priming packet
    # already carries, which the muxer preserves via an edit list.
    for node in graph.nodes:
        stream_packets = packets_by_stream.get(node.source.index)
        first = next((p for p in (stream_packets or ()) if p.pts is not None), None)
        if first is None:
            continue
        first_seconds = Fraction(first.pts) * first.time_base
        frame = (Fraction(first.duration) * first.time_base) if first.duration else Fraction(0)
        allowed = first_seconds + frame
        if origin_seconds > allowed:
            origin_seconds = allowed

    shifts: dict[int, int] = {}
    snapped: dict[int, Fraction] = {}
    for node in graph.nodes:
        stream_packets = packets_by_stream.get(node.source.index)
        if not stream_packets:
            shifts[node.source.index] = 0
            snapped[node.source.index] = Fraction(0)
            continue
        tb = stream_packets[0].time_base
        # The shift must be an integer tick count, so the applied instant is
        # quantised to this stream's time base. Quantisation error is at most
        # half a tick (~10 µs for 48 kHz audio), far below the sync budget, but
        # it means "the same instant" is enforced only to within one tick.
        ticks = round(origin_seconds / tb)
        shifts[node.source.index] = ticks
        snapped[node.source.index] = ticks * tb
    spread = max(snapped.values()) - min(snapped.values())
    return origin_seconds, shifts, spread


def remux_asset(
    probe: AssetProbe,
    graph: TrackGraph,
    packets: list[RawPacket],
    destination: Path,
) -> RemuxResult:
    """Write ``packets`` into ``destination`` as a lossless copy.

    ``destination`` must be a fresh path inside the caller's temp directory;
    the caller atomically renames it into place after verification.
    """
    check_lossless_support([node.source for node in graph.nodes])

    by_stream: dict[int, list[RawPacket]] = {}
    for packet in packets:
        by_stream.setdefault(packet.stream, []).append(packet)

    declared = {
        node.source.index: node.source.start_time
        for node in graph.nodes
        if node.source.start_time is not None
    }
    origin, shifts, origin_spread = _origin_ticks(graph, by_stream, declared)
    fmt = format_guess(destination)
    stats: dict[int, OutputTrackStats] = {
        node.output: OutputTrackStats(output=node.output, source=node.source.index)
        for node in graph.nodes
    }
    digests: dict[int, "hashlib._Hash"] = {
        node.output: hashlib.sha256() for node in graph.nodes
    }

    # Open the source only to obtain stream templates; the packets themselves
    # come from the already-recovered list.
    try:
        source_container = av.open(str(probe.path), mode="r", metadata_errors="ignore")
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        raise RemuxError(
            f"cannot reopen input for templates: {exc}",
            location=MediaLocation(asset=str(probe.path)),
        ) from exc
    try:
        source_streams = {stream.index: stream for stream in source_container.streams}
        destination.parent.mkdir(parents=True, exist_ok=True)
        with av.open(
            str(destination),
            mode="w",
            format=fmt,
            options={"movflags": "+faststart"},
        ) as out:
            outputs: dict[int, object] = {}
            stream_map: dict[int, int] = {}
            for node in graph.ordered():
                source_index = node.source.index
                template = source_streams.get(source_index)
                if template is None:
                    raise RemuxError(
                        f"source stream {source_index} vanished before remux",
                        location=MediaLocation(
                            asset=str(probe.path), stream=source_index, rule=node.output
                        ),
                    )
                output_stream = out.add_stream(template=template)
                outputs[node.output] = output_stream
                stream_map[source_index] = output_stream.index
            out.metadata["encoder"] = "bcastqc lossless remux"
            out.metadata["comment"] = "packet copy; byte-identical payloads"

            for node in graph.ordered():
                source_index = node.source.index
                output_stream = outputs[node.output]
                shift = shifts.get(source_index, 0)
                bucket = stats[node.output]
                digest = digests[node.output]
                for packet in by_stream.get(source_index, ()):
                    raw = av.Packet(packet.data)
                    pts, dts = _packet_times(packet, shift, packet.time_base)
                    raw.pts = pts
                    raw.dts = dts
                    raw.duration = packet.duration
                    raw.is_keyframe = packet.keyframe
                    raw.time_base = packet.time_base
                    raw.stream = output_stream  # type: ignore[assignment]
                    out.mux(raw)
                    bucket.packets += 1
                    bucket.bytes += len(packet.data)
                    if packet.keyframe:
                        bucket.keyframes += 1
                    digest.update(len(packet.data).to_bytes(8, "big"))
                    digest.update(packet.data)
            # (4) closing the container writes the index, after all packets.
    except av.error.FFmpegError as exc:
        raise LosslessCopyRefusedError(
            f"container refused the lossless copy: {exc}",
            location=MediaLocation(asset=str(probe.path), detail=str(exc)),
        ) from exc
    finally:
        source_container.close()

    for output, digest in digests.items():
        stats[output].digest = digest.hexdigest()
    try:
        size = destination.stat().st_size
    except OSError as exc:
        raise RemuxError(f"output missing after remux: {exc}") from exc
    return RemuxResult(
        output_path=destination,
        bytes_written=size,
        packets_written=sum(s.packets for s in stats.values()),
        per_track=[stats[node.output] for node in graph.ordered()],
        origin=origin,
        origin_spread=origin_spread,
        shifts=shifts,
        stream_map=stream_map,
    )


@dataclass(slots=True)
class AtomicOutput:
    """A temp file that only becomes visible on :meth:`commit`."""

    final_path: Path
    temp_path: Path
    committed: bool = False

    def commit(self) -> None:
        if self.committed:
            return
        self.final_path.parent.mkdir(parents=True, exist_ok=True)
        os.replace(self.temp_path, self.final_path)
        self.committed = True

    def discard(self) -> None:
        if self.committed:
            return
        try:
            self.temp_path.unlink()
        except FileNotFoundError:
            pass


def atomic_output(directory: Path, name: str) -> AtomicOutput:
    directory.mkdir(parents=True, exist_ok=True)
    handle, temp = tempfile.mkstemp(prefix=f".{name}.", suffix=".part", dir=directory)
    os.close(handle)
    return AtomicOutput(final_path=directory / name, temp_path=Path(temp))


def packet_digest(packets: list[RawPacket]) -> str:
    digest = hashlib.sha256()
    for packet in packets:
        digest.update(len(packet.data).to_bytes(8, "big"))
        digest.update(packet.data)
    return digest.hexdigest()
