"""Lossless remux writer.

Packets are forwarded with their original ``pts``/``dts``/``time_base`` and
payload untouched; only the container changes.  Each packet's stream is
re-pointed at the corresponding output stream created from the input template,
which copies codec parameters (including extradata) so the compressed bitstream
stays decodable exactly as before.

The writer is deliberately *not* a transcoder: it never touches frame data, and
it rejects any attempt to remux to a container whose muxer cannot carry one of
the selected codecs, raising with the stream index instead of silently dropping
it.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av

from .demux import DemuxedPacket
from .mapping import Mapping
from .probe import ProbeResult
from .timebase import (
    InputError,
    MappingError,
    UnrecoverableMuxError,
)


#: Container extensions the writer knows how to target.
TARGET_FORMATS: dict[str, str] = {
    "mov": "mov",
    "mp4": "mp4",
    "mkv": "matroska",
}


@dataclass
class RemuxStats:
    packets_in: int = 0
    packets_out: int = 0
    bytes_out: int = 0
    per_output_stream: dict[int, int] = field(default_factory=dict)
    last_segment: int = 0
    skipped: int = 0

    def to_dict(self) -> dict[str, object]:
        return {
            "packets_in": self.packets_in,
            "packets_out": self.packets_out,
            "bytes_out": self.bytes_out,
            "per_output_stream": {str(k): v for k, v in sorted(self.per_output_stream.items())},
            "last_segment": self.last_segment,
            "skipped": self.skipped,
        }


@dataclass
class RemuxWriter:
    """Stream one asset's packets into a target container.

    ``target`` may be ``"mov"``, ``"mp4"`` or ``"mkv"``; the extension is added
    by the caller.  ``mapping`` selects which input streams are copied and in
    what output order.
    """

    source: ProbeResult
    mapping: Mapping
    target_format: str
    path: Path
    stats: RemuxStats = field(default_factory=RemuxStats)

    _container: av.container.OutputContainer | None = None
    _stream_map: dict[int, av.stream.Stream] = field(default_factory=dict)
    _opened: bool = False

    def open(self) -> None:
        if self.target_format not in TARGET_FORMATS:
            raise InputError(
                "unsupported target container",
                target=self.target_format,
                supported=sorted(TARGET_FORMATS),
            )
        if self.path.exists():
            raise InputError(
                "refusing to overwrite an existing output file",
                path=str(self.path),
            )
        self.path.parent.mkdir(parents=True, exist_ok=True)
        options: dict[str, str] = {}
        if self.target_format in ("mov", "mp4"):
            options["movflags"] = "+faststart"
        try:
            self._container = av.open(
                str(self.path), "w", format=TARGET_FORMATS[self.target_format], options=options
            )
        except av.error.FFmpegError as exc:
            raise InputError(
                "target container could not be opened",
                path=str(self.path),
                format=self.target_format,
                error=str(exc),
            ) from exc

        try:
            input_container = av.open(str(self.source.path))
        except av.error.FFmpegError as exc:
            self._container.close()
            self._container = None
            raise InputError(
                "source could not be reopened for remux",
                path=str(self.source.path),
                error=str(exc),
            ) from exc

        try:
            input_streams = {stream.index: stream for stream in input_container.streams}
            for assignment in self.mapping.assignments:
                source_stream = input_streams.get(assignment.input_index)
                if source_stream is None:
                    raise MappingError(
                        "mapped source stream disappeared between probe and remux",
                        stream_index=assignment.input_index,
                    )
                try:
                    output_stream = self._container.add_stream(template=source_stream)
                except (av.error.FFmpegError, ValueError) as exc:
                    raise UnrecoverableMuxError(
                        "codec cannot be carried by the target container",
                        stream_index=assignment.input_index,
                        kind=assignment.kind,
                        codec=source_stream.codec_context.name,
                        target=self.target_format,
                        error=str(exc),
                    ) from exc
                self._stream_map[assignment.input_index] = output_stream
        finally:
            input_container.close()
        self._opened = True

    def write(self, packet: DemuxedPacket) -> bool:
        """Write one packet; returns ``False`` if its stream was not mapped."""
        if not self._opened or self._container is None:
            raise RuntimeError("writer must be opened before write")
        output_stream = self._stream_map.get(packet.stream_index)
        if output_stream is None:
            self.stats.skipped += 1
            return False
        rebuilt = av.Packet(packet.payload)
        rebuilt.stream = output_stream
        rebuilt.time_base = Fraction(*packet.time_base)
        # Re-emit the original raw timestamps.  The canonical nanosecond ticks
        # are a validation clock; converting a packet to them and back could
        # round, so the byte-exact copy always uses the values the demuxer
        # produced.
        rebuilt.pts = packet.pts_raw
        rebuilt.dts = packet.dts_raw
        rebuilt.is_keyframe = packet.is_keyframe
        try:
            self._container.mux(rebuilt)
        except av.error.FFmpegError as exc:
            raise UnrecoverableMuxError(
                "muxing failed for packet",
                stream_index=packet.stream_index,
                segment=packet.segment,
                pts=packet.pts_raw,
                dts=packet.dts_raw,
                error=str(exc),
            ) from exc
        self.stats.packets_out += 1
        self.stats.bytes_out += packet.size
        self.stats.per_output_stream[output_stream.index] = (
            self.stats.per_output_stream.get(output_stream.index, 0) + 1
        )
        self.stats.last_segment = max(self.stats.last_segment, packet.segment)
        return True

    def close(self) -> None:
        if self._container is not None:
            try:
                self._container.close()
            finally:
                self._container = None
                self._opened = False

    def __enter__(self) -> "RemuxWriter":
        self.open()
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()


__all__ = ["RemuxStats", "RemuxWriter", "TARGET_FORMATS"]
