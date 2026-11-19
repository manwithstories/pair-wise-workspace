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
    shifts: dict[int, int] = field(default_factory=dict)
    stream_map: dict[int, int] = field(default_factory=dict)

    def to_dict(self) -> dict[str, object]:
        return {
            "output_path": str(self.output_path),
            "bytes_written": self.bytes_written,
            "packets_written": self.packets_written,
            "origin": round(float(self.origin), 9),
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
    graph: TrackGraph, packets_by_stream: dict[int, list[RawPacket]]
) -> tuple[Fraction, dict[int, int]]:
    """Pick a shared origin (in ticks) and the per-stream integer shift.

    Streams are shifted by the *same presentation instant*; the tick value
    differs per stream because each stream has its own time base.
    """
    candidates: list[tuple[Fraction, int]] = []
    for node in graph.ordered():
        stream_packets = packets_by_stream.get(node.source.index, [])
        first = next(
            (p for p in stream_packets if p.pts is not None),
            None,
        )
        if first is not None:
            candidates.append((Fraction(first.pts) * first.time_base, node.source.index))
    if not candidates:
        return Fraction(0), {node.source.index: 0 for node in graph.nodes}
    origin_seconds = min(c[0] for c in candidates)
    shifts: dict[int, int] = {}
    for node in graph.nodes:
        stream_packets = packets_by_stream.get(node.source.index)
        if not stream_packets:
            shifts[node.source.index] = 0
            continue
        tb = stream_packets[0].time_base
        shifts[node.source.index] = round(origin_seconds / tb)
    return origin_seconds, shifts


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

    origin, shifts = _origin_ticks(graph, by_stream)
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
