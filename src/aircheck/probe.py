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
    open_error: str | None = None
    index_info: dict[str, object] = field(default_factory=dict)

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
    def unreadable(self) -> bool:
        """True when the container could not be opened at all."""
        return self.open_error is not None and not self.streams

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
            "open_error": self.open_error,
            "index_info": dict(self.index_info),
            "has_index": self.has_index,
            "truncated": self.truncated,
            "unreadable": self.unreadable,
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


# EBML readers shared by the structural checks below.  Element IDs keep the
# length-marker bit (that is how IDs are specified), while data sizes must have
# it stripped before the value is usable.
def _read_vint(data: bytes, position: int) -> tuple[int | None, int]:
    """Read an EBML element ID, keeping its length marker."""
    return _read_vint_raw(data, position, keep_marker=True)


def _read_size(data: bytes, position: int) -> tuple[int | None, int]:
    """Read an EBML data size, stripping its length marker."""
    return _read_vint_raw(data, position, keep_marker=False)


def _read_vint_raw(data: bytes, position: int, *, keep_marker: bool) -> tuple[int | None, int]:
    if position >= len(data):
        return None, 0
    first = data[position]
    if first == 0:
        return None, 0
    width = 1
    mask = 0x80
    while width <= 8 and not first & mask:
        mask >>= 1
        width += 1
    if width > 8 or position + width > len(data):
        return None, 0
    if keep_marker:
        value = first
    else:
        value = first & (mask - 1)
    for offset in range(1, width):
        value = (value << 8) | data[position + offset]
    return value, width


def _mkv_index_health(path: Path) -> tuple[bool, list[str], int | None]:
    """Walk a Matroska file's top-level structure and inspect its Cues.

    Returns ``(has_cues, problems, cue_count)``.  ``problems`` contains
    stable tokens - ``no_cues``, ``duplicate_cue``, ``cue_out_of_order``,
    ``cue_past_eof`` - that the CLI surfaces verbatim.  Structural parse
    failures degrade to ``(False, ["no_cues"], None)`` rather than raising:
    index health must never be inferred from a best-effort parse that crashed.
    """
    try:
        data = path.read_bytes()
    except OSError:
        return False, ["no_cues"], None
    if not data.startswith(b"\x1a\x45\xdf\xa3"):
        return False, ["no_cues"], None

    position = 0
    # Skip the EBML header element.
    _, header_id_len = _read_vint(data, position)
    header_size, header_size_len = _read_size(data, position + header_id_len)
    if header_size is None:
        return False, ["no_cues"], None
    # The EBML header is a regular sized element, so the Segment ID follows
    # immediately at its declared end; no padding to skip.
    position += header_id_len + header_size_len + header_size

    segment_id, id_len = _read_vint(data, position)
    if segment_id is None:
        return False, ["no_cues"], None
    segment_size, size_len = _read_size(data, position + id_len)
    if segment_size is None:
        return False, ["no_cues"], None
    body_start = position + id_len + size_len
    unknown_size = segment_size == (1 << (7 * size_len)) - 1
    body_end = len(data) if unknown_size else min(len(data), body_start + segment_size)

    cues_payload: bytes | None = None
    cursor = body_start
    while cursor < body_end:
        element_id, element_id_len = _read_vint(data, cursor)
        if element_id is None:
            break
        element_size, element_size_len = _read_size(data, cursor + element_id_len)
        if element_size is None:
            break
        payload_start = cursor + element_id_len + element_size_len
        payload_end = min(len(data), payload_start + element_size)
        if element_id == 0x1C53BB6B:  # Cues
            cues_payload = data[payload_start:payload_end]
            break
        cursor = payload_start + element_size

    if cues_payload is None:
        return False, ["no_cues"], None

    times: list[int] = []
    offenders: set[str] = set()
    cue_entries: list[dict[str, object]] = []
    cursor = 0
    header_size_total = len(data)
    while cursor < len(cues_payload):
        element_id, element_id_len = _read_vint(cues_payload, cursor)
        if element_id is None:
            break
        element_size, element_size_len = _read_size(cues_payload, cursor + element_id_len)
        if element_size is None:
            break
        if element_id != 0xBB:  # CuePoint; skip other Cues children (e.g. CRC-32)
            cursor = cursor + element_id_len + element_size_len + element_size
            continue
        point_start = cursor + element_id_len + element_size_len
        point_end = min(len(cues_payload), point_start + element_size)
        time_value, position_value = _parse_cue_point(cues_payload, point_start, point_end)
        cue_entries.append({"time": time_value, "cluster_position": position_value})
        if time_value is not None:
            if times and time_value < times[-1]:
                offenders.add("cue_out_of_order")
            elif time_value in times:
                offenders.add("duplicate_cue")
            times.append(time_value)
        if position_value is not None and position_value >= header_size_total:
            offenders.add("cue_past_eof")
        cursor = point_start + element_size

    order = {
        "duplicate_cue": 0,
        "cue_out_of_order": 1,
        "cue_past_eof": 2,
    }
    _MKV_CUE_CACHE[str(path)] = cue_entries
    return True, sorted(offenders, key=lambda name: order.get(name, 99)), len(times)


#: Raw Matroska cue entries keyed by file path, so the keyframe validator can
#: check the container's *own* index instead of only re-scanning packets.
_MKV_CUE_CACHE: dict[str, list[dict[str, object]]] = {}


def mkv_cue_entries(path: Path) -> list[dict[str, object]]:
    """Return the Matroska cue entries for ``path`` (empty for other formats).

    Entries are ``{"time": int | None, "cluster_position": int | None}`` in
    file order, including entries a well-behaved muxer would never write.
    """
    key = str(path)
    if key not in _MKV_CUE_CACHE:
        _mkv_index_health(path)
    return list(_MKV_CUE_CACHE.get(key, []))


def mkv_cue_scale_ns(path: Path) -> int:
    """Timestamp scale of the Matroska segment (nanoseconds per tick)."""
    try:
        data = path.read_bytes()
    except OSError:
        return 1_000_000
    marker = b"\x2a\xd7\xb1"  # TimestampScale
    position = data.find(marker)
    if position < 0:
        return 1_000_000
    size, size_len = _read_size(data, position + len(marker))
    if not size:
        return 1_000_000
    start = position + len(marker) + size_len
    return int.from_bytes(data[start:start + size], "big") or 1_000_000


def mkv_segment_start(path: Path) -> int:
    """Byte offset of the Segment's body, needed to resolve cue positions."""
    try:
        data = path.read_bytes()
    except OSError:
        return 0
    if not data.startswith(b"\x1a\x45\xdf\xa3"):
        return 0
    _, id_len = _read_vint(data, 0)
    header_size, size_len = _read_size(data, id_len)
    if header_size is None:
        return 0
    position = id_len + size_len + header_size
    segment_id, segment_id_len = _read_vint(data, position)
    if segment_id is None:
        return 0
    _, segment_size_len = _read_size(data, position + segment_id_len)
    return position + segment_id_len + segment_size_len


def _parse_cue_point(payload: bytes, start: int, end: int) -> tuple[int | None, int | None]:
    time_value: int | None = None
    position_value: int | None = None
    cursor = start
    while cursor < end:
        element_id, element_id_len = _read_vint(payload, cursor)
        if element_id is None:
            break
        element_size, element_size_len = _read_size(payload, cursor + element_id_len)
        if element_size is None:
            break
        child_start = cursor + element_id_len + element_size_len
        child_end = min(end, child_start + element_size)
        if element_id == 0xB3:  # CueTime
            time_value = int.from_bytes(payload[child_start:child_end], "big")
        elif element_id == 0xB7:  # CueTrackPositions
            position_value = _parse_cue_track_positions(payload, child_start, child_end)
        cursor = child_start + element_size
    return time_value, position_value


def _parse_cue_track_positions(payload: bytes, start: int, end: int) -> int | None:
    position: int | None = None
    cursor = start
    while cursor < end:
        element_id, element_id_len = _read_vint(payload, cursor)
        if element_id is None:
            break
        element_size, element_size_len = _read_size(payload, cursor + element_id_len)
        if element_size is None:
            break
        child_start = cursor + element_id_len + element_size_len
        child_end = min(end, child_start + element_size)
        if element_id == 0xF1:  # CueClusterPosition
            position = int.from_bytes(payload[child_start:child_end], "big")
        cursor = child_start + element_size
    return position


def _iso_bmff_atoms(path: Path) -> list[str] | None:
    """Return the top-level atom types of an ISO-BMFF file, or ``None``.

    Used to tell a tail-truncated MP4/MOV (``moov`` index lost) from a file
    that was never a valid container: a readable ``ftyp`` followed by a missing
    ``moov`` is the classic truncation signature.
    """
    try:
        with path.open("rb") as handle:
            data = handle.read(1 << 16)
    except OSError:
        return None
    if len(data) < 8 or data[4:8] != b"ftyp":
        return None
    atoms: list[str] = []
    position = 0
    size_total = path.stat().st_size
    try:
        handle = path.open("rb")
    except OSError:
        return None
    with handle:
        while position + 8 <= size_total:
            handle.seek(position)
            header = handle.read(16)
            if len(header) < 8:
                break
            size = int.from_bytes(header[0:4], "big")
            atom = header[4:8].decode("latin1", "replace")
            atoms.append(atom)
            if size == 1 and len(header) >= 16:
                size = int.from_bytes(header[8:16], "big")
            if size < 8:
                break
            position += size
    return atoms


def _classify_open_failure(path: Path) -> ProbeIssue:
    atoms = _iso_bmff_atoms(path)
    if atoms is not None and "moov" not in atoms:
        return ProbeIssue(
            "truncated",
            "ISO-BMFF file has ftyp but no moov atom; the index at the tail was lost",
        )
    return ProbeIssue("corrupt", "container header could not be parsed")


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
        issue = _classify_open_failure(source)
        return ProbeResult(
            path=source,
            format_name="",
            format_long_name="",
            duration_ticks=None,
            start_ticks=None,
            bit_rate=None,
            size_bytes=size_bytes,
            streams=[],
            issues=[issue],
            packet_counts={},
            container_metadata={},
            open_error=str(exc),
        )

    try:
        format_name = container.format.name or ""
        format_long_name = container.format.long_name or format_name
        index_info: dict[str, object] = {}
        # Container-level duration/start use av.time_base (microseconds).
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
            has_cues, problems, cue_count = _mkv_index_health(source)
            index_info = {
                "has_cues": has_cues,
                "problems": problems,
                "cue_count": cue_count,
                "cue_entries": mkv_cue_entries(source) if has_cues else [],
                "cue_scale_ns": mkv_cue_scale_ns(source),
                "segment_start": mkv_segment_start(source),
            }
            if not has_cues:
                issues.append(
                    ProbeIssue(
                        "no_index",
                        "Matroska segment has no Cues element; seeking degrades to linear scan",
                    )
                )
            for problem in problems:
                if problem == "no_cues":
                    continue
                issues.append(
                    ProbeIssue(
                        "index_corrupt",
                        f"Matroska Cues table is unusable: {problem}",
                    )
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
                        if packet.is_corrupt:
                            issues.append(
                                ProbeIssue(
                                    "corrupt",
                                    f"demuxer flagged corrupted packet at pts={packet.pts}",
                                    stream_index=packet.stream.index,
                                )
                            )
                        if decode_check:
                            try:
                                for frame in packet.decode():
                                    if getattr(frame, "is_corrupt", False):
                                        issues.append(
                                            ProbeIssue(
                                                "corrupt",
                                                f"decoded frame marked corrupt at pts={packet.pts}",
                                                stream_index=packet.stream.index,
                                            )
                                        )
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
        index_info=index_info,
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
    "mkv_cue_entries",
    "mkv_cue_scale_ns",
    "mkv_segment_start",
    "probe",
    "require_media",
]
