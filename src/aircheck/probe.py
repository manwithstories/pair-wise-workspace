"""Container and stream probing.

Reads structural metadata only (no payload decoding) and normalises it into
plain dataclasses that the rest of the pipeline can consume without holding a
live ``av`` container open.

Damage classes recognised here:

* ``truncated``  - the file ends before the container's declared extent, or the
  demuxer raises EOF mid-payload;
* ``no_index``   - the container offers no seekable keyframe index (e.g. a
  Matroska file without ``Cues``), so keyframe lookup must degrade;
* ``corrupt``    - the demuxer reports invalid data while walking packets.

Probing never raises for recoverable damage: it records findings on the
:class:`ProbeResult` so the caller can decide whether to quarantine the asset
or continue.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av

from .timebase import (
    AircheckError,
    InputError,
    TimestampTracker,
    as_fraction,
    to_ticks,
)

INPUT_EXTENSIONS = {".mov", ".mp4", ".m4v", ".mkv", ".mka", ".webm"}


@dataclass(frozen=True)
class StreamInfo:
    """Normalised metadata for one elementary stream."""

    index: int
    kind: str                 # "video" | "audio" | "other"
    codec: str
    time_base: Fraction
    duration_ticks: int | None
    start_ticks: int | None
    language: str | None = None
    width: int | None = None
    height: int | None = None
    frame_rate: Fraction | None = None
    sample_rate: int | None = None
    channels: int | None = None
    metadata: dict[str, str] = field(default_factory=dict)

    @property
    def is_media(self) -> bool:
        return self.kind in ("video", "audio")

    def to_dict(self) -> dict[str, object]:
        return {
            "index": self.index,
            "kind": self.kind,
            "codec": self.codec,
            "time_base": str(self.time_base),
            "duration_ticks": self.duration_ticks,
            "start_ticks": self.start_ticks,
            "language": self.language,
            "width": self.width,
            "height": self.height,
            "frame_rate": str(self.frame_rate) if self.frame_rate else None,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "metadata": dict(self.metadata),
        }


@dataclass(frozen=True)
class ProbeIssue:
    kind: str
    detail: str
    stream_index: int | None = None

    def to_dict(self) -> dict[str, object]:
        return {"kind": self.kind, "detail": self.detail, "stream_index": self.stream_index}


@dataclass
class ProbeResult:
    """Everything the pipeline needs to route one input asset."""

    path: Path
    format_name: str
    format_long_name: str
    duration_ticks: int | None
    start_ticks: int | None
    bit_rate: int | None
    size_bytes: int
    streams: list[StreamInfo]
    issues: list[ProbeIssue] = field(default_factory=list)
    packet_counts: dict[int, int] = field(default_factory=dict)
    container_metadata: dict[str, str] = field(default_factory=dict)
    observed_duration_ticks: int | None = None

    @property
    def media_streams(self) -> list[StreamInfo]:
        return [stream for stream in self.streams if stream.is_media]

    @property
    def video_streams(self) -> list[StreamInfo]:
        return [stream for stream in self.streams if stream.kind == "video"]

    @property
    def audio_streams(self) -> list[StreamInfo]:
        return [stream for stream in self.streams if stream.kind == "audio"]

    @property
    def truncated(self) -> bool:
        return any(issue.kind == "truncated" for issue in self.issues)

    @property
    def has_index(self) -> bool:
        return not any(issue.kind == "no_index" for issue in self.issues)

    def issue_kinds(self) -> set[str]:
        return {issue.kind for issue in self.issues}

    def to_dict(self) -> dict[str, object]:
        return {
            "path": str(self.path),
            "format": self.format_name,
            "format_long_name": self.format_long_name,
            "duration_ticks": self.duration_ticks,
            "start_ticks": self.start_ticks,
            "bit_rate": self.bit_rate,
            "size_bytes": self.size_bytes,
            "streams": [stream.to_dict() for stream in self.streams],
            "issues": [issue.to_dict() for issue in self.issues],
            "packet_counts": {str(k): v for k, v in sorted(self.packet_counts.items())},
            "container_metadata": dict(self.container_metadata),
            "observed_duration_ticks": self.observed_duration_ticks,
            "has_index": self.has_index,
            "truncated": self.truncated,
        }


def _classify(stream: av.stream.Stream) -> str:
    if stream.type == "video":
        return "video"
    if stream.type == "audio":
        return "audio"
    return "other"


def _stream_info(stream: av.stream.Stream) -> StreamInfo:
    codec_context = stream.codec_context
    time_base = as_fraction(stream.time_base) if stream.time_base else Fraction(0, 1)
    kind = _classify(stream)
    duration_ticks = to_ticks(stream.duration, time_base) if stream.duration else None
    start_ticks = to_ticks(stream.start_time, time_base) if stream.start_time is not None else None
    info = StreamInfo(
        index=stream.index,
        kind=kind,
        codec=codec_context.name,
        time_base=time_base,
        duration_ticks=duration_ticks,
        start_ticks=start_ticks,
        language=None,
        metadata=dict(stream.metadata or {}),
    )
    if kind == "video":
        info = StreamInfo(
            **{
                **info.__dict__,
                "width": codec_context.width,
                "height": codec_context.height,
                "frame_rate": Fraction(stream.average_rate) if stream.average_rate else None,
            }
        )
    elif kind == "audio":
        info = StreamInfo(
            **{
                **info.__dict__,
                "sample_rate": codec_context.sample_rate or None,
                "channels": codec_context.channels or None,
            }
        )
    metadata = dict(stream.metadata or {})
    language = metadata.get("language")
    if language:
        info = StreamInfo(**{**info.__dict__, "language": language})
    return info


def _mkv_has_cues(path: Path) -> bool | None:
    """Cheap structural check for a Matroska ``Cues`` element.

    Returns ``None`` when the file is not Matroska or the structure cannot be
    walked, so the caller can fall back to the demuxer's own behaviour.
    """
    try:
        with path.open("rb") as handle:
            data = handle.read(4 << 20)
    except OSError:
        return None
    if not data.startswith(b"\x1a\x45\xdf\xa3"):
        return None
    return b"\x1c\x53\xbb\x6b" in data


def probe(
    path: str | Path,
    *,
    scan_packets: bool = True,
    decode_check: bool = False,
    limit: int | None = None,
) -> ProbeResult:
    """Read metadata for ``path``.

    ``scan_packets`` walks the demuxer once (bounded by ``limit`` packets if
    given) to count packets, validate timestamps and classify damage.  With
    ``scan_packets=False`` only headers are read, which is cheap but cannot
    detect truncation or corruption.  ``decode_check`` additionally decodes
    every demuxed packet so payload-level corruption (which packet demuxers do
    not validate) is surfaced as a ``corrupt`` issue; it is slower and is only
    requested for damaged-asset classification.
    """
    source = Path(path)
    if not source.exists():
        raise InputError("input file does not exist", path=str(source))
    if not source.is_file():
        raise InputError("input path is not a regular file", path=str(source))
    try:
        size_bytes = source.stat().st_size
    except OSError as exc:
        raise InputError("cannot stat input file", path=str(source), error=str(exc)) from exc
    if size_bytes == 0:
        raise InputError("input file is empty", path=str(source))

    issues: list[ProbeIssue] = []
    packet_counts: dict[int, int] = {}
    try:
        container = av.open(str(source))
    except av.error.FFmpegError as exc:
        raise InputError(
            "container could not be opened",
            path=str(source),
            error=str(exc),
            size_bytes=size_bytes,
        ) from exc

    try:
        format_name = container.format.name or ""
        format_long_name = container.format.long_name or format_name
        time_base = Fraction(1, 1_000_000) if container.format.name else Fraction(1, 1)
        duration_ticks = (
            to_ticks(container.duration, Fraction(1, 1_000_000))
            if container.duration is not None
            else None
        )
        start_ticks = (
            to_ticks(container.start_time, Fraction(1, 1_000_000))
            if container.start_time is not None
            else None
        )
        streams = [_stream_info(stream) for stream in container.streams]
        container_metadata = dict(container.metadata or {})
        bit_rate = container.bit_rate

        if "matroska" in format_name or "webm" in format_name:
            cues = _mkv_has_cues(source)
            if cues is False:
                issues.append(
                    ProbeIssue("no_index", "Matroska segment has no Cues element; seeking degrades to linear scan")
                )

        if scan_packets:
            trackers = {
                stream.index: TimestampTracker(stream.index, stream.time_base)
                for stream in streams
                if stream.is_media
            }
            last_ticks: dict[int, int] = {}
            seen = 0
            eof_cleanly = False
            demux_error: str | None = None
            try:
                for packet in container.demux():
                    if packet.size == 0:
                        continue
                    seen += 1
                    packet_counts[packet.stream.index] = packet_counts.get(packet.stream.index, 0) + 1
                    tracker = trackers.get(packet.stream.index)
                    if tracker is not None:
                        for issue in tracker.observe(packet.pts, packet.dts):
                            issues.append(
                                ProbeIssue(
                                    issue.kind,
                                    issue.detail,
                                    stream_index=issue.stream_index,
                                )
                            )
                        if packet.pts is not None and packet.pts >= 0:
                            end = to_ticks(packet.pts, tracker.time_base)
                            if end is not None:
                                last_ticks[packet.stream.index] = max(
                                    last_ticks.get(packet.stream.index, 0), end
                                )
                        if decode_check:
                            try:
                                for _frame in packet.decode():
                                    pass
                            except av.error.FFmpegError as exc:
                                issues.append(
                                    ProbeIssue(
                                        "corrupt",
                                        f"decode failed at pts={packet.pts}: {exc}",
                                        stream_index=packet.stream.index,
                                    )
                                )
                    if limit is not None and seen >= limit:
                        break
                else:
                    eof_cleanly = True
            except av.error.EOFError:
                eof_cleanly = True
            except av.error.FFmpegError as exc:
                demux_error = str(exc)
                issues.append(ProbeIssue("corrupt", f"demux failed: {exc}"))

            if limit is None:
                if demux_error is not None:
                    # A mid-stream demux failure leaves an unknown tail.
                    issues.append(
                        ProbeIssue(
                            "truncated",
                            f"demux stopped early: {demux_error}",
                        )
                    )
                elif eof_cleanly and duration_ticks and last_ticks:
                    # Compare the furthest demuxed timestamp with the header's
                    # declared duration.  A short tail (<97%) means bytes were
                    # lost even though the demuxer reached EOF quietly.
                    # Wrapped or disordered timelines make this comparison
                    # meaningless, so it is skipped in that case.
                    stable = not any(
                        issue.kind in ("wrap", "non_monotonic") for issue in issues
                    )
                    observed_end = max(last_ticks.values())
                    if stable and observed_end + duration_ticks // 33 < duration_ticks:
                        issues.append(
                            ProbeIssue(
                                "truncated",
                                (
                                    f"payload ends at {observed_end} ticks but the header "
                                    f"declares {duration_ticks} ticks"
                                ),
                            )
                        )
            if any(issue.kind in ("wrap", "non_monotonic") for issue in issues):
                # Timeline was discontinuous; the raw maximum is not a duration.
                observed_duration = None
            else:
                observed_duration = max(last_ticks.values()) if last_ticks else None
        else:
            observed_duration = None
    finally:
        container.close()

    return ProbeResult(
        path=source,
        format_name=format_name,
        format_long_name=format_long_name,
        duration_ticks=duration_ticks,
        start_ticks=start_ticks,
        bit_rate=bit_rate,
        size_bytes=size_bytes,
        streams=streams,
        issues=_dedupe_issues(issues),
        packet_counts=packet_counts,
        container_metadata=container_metadata,
        observed_duration_ticks=observed_duration,
    )


def _dedupe_issues(issues: list[ProbeIssue]) -> list[ProbeIssue]:
    """Collapse repeated timestamp findings to one entry per (kind, stream)."""
    seen: set[tuple[str, int | None]] = set()
    out: list[ProbeIssue] = []
    for issue in issues:
        key = (issue.kind, issue.stream_index)
        if key in seen:
            continue
        seen.add(key)
        out.append(issue)
    return out


def require_media(result: ProbeResult) -> None:
    """Raise when a probe found no usable audio or video stream."""
    if not result.media_streams:
        raise InputError(
            "no audio or video streams found",
            path=str(result.path),
            stream_count=len(result.streams),
        )


__all__ = [
    "INPUT_EXTENSIONS",
    "ProbeIssue",
    "ProbeResult",
    "StreamInfo",
    "AircheckError",
    "probe",
    "require_media",
]
