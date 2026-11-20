"""Container, stream and keyframe probing with PyAV.

The probe is the only module that talks to libavformat directly.  It streams
packets (never decoded frames) and keeps only compact per-packet metadata, so a
long master does not have to fit in memory.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterable

import av

from . import __version__
from .errors import MediaError, bridge
from .timeline import (
    Rational,
    TimeBase,
    analyze_timestamps,
    choose_timeline_rate,
)
from .util import sha256_file

ALLOWED_FORMATS = {"mov", "mp4", "m4a", "matroska", "webm"}
FORMAT_LABEL = {
    "mov": "mov", "mp4": "mp4", "m4a": "mp4", "matroska": "mkv", "webm": "mkv",
}

#: Two or more log lines that mean "the file was cut short" even when the
#: container header still declares the original duration.
_TRUNCATION_MARKERS = ("File ended prematurely", "Invalid track number", "Truncated")


@dataclass(slots=True)
class PacketMeta:
    """Compact per-packet record; no payload is retained."""

    index: int
    pts: int | None
    dts: int | None
    duration: int | None
    size: int
    offset: int | None
    keyframe: bool
    corrupt: bool

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "pts": self.pts,
            "dts": self.dts,
            "duration": self.duration,
            "size": self.size,
            "offset": self.offset,
            "keyframe": self.keyframe,
            "corrupt": self.corrupt,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "PacketMeta":
        return cls(
            index=int(data["index"]),
            pts=None if data.get("pts") is None else int(data["pts"]),
            dts=None if data.get("dts") is None else int(data["dts"]),
            duration=None if data.get("duration") is None else int(data["duration"]),
            size=int(data.get("size", 0)),
            offset=None if data.get("offset") is None else int(data["offset"]),
            keyframe=bool(data.get("keyframe", False)),
            corrupt=bool(data.get("corrupt", False)),
        )


@dataclass(slots=True)
class Keyframe:
    ticks: int
    offset: int | None
    size: int
    pts: int | None

    def to_dict(self) -> dict[str, Any]:
        return {
            "ticks": self.ticks,
            "offset": self.offset,
            "size": self.size,
            "pts": self.pts,
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "Keyframe":
        return cls(
            ticks=int(data["ticks"]),
            offset=None if data.get("offset") is None else int(data["offset"]),
            size=int(data["size"]),
            pts=None if data.get("pts") is None else int(data["pts"]),
        )


@dataclass(slots=True)
class StreamProbe:
    index: int
    kind: str
    codec: str
    time_base: Rational
    start_raw: int | None = None
    duration_raw: int | None = None
    sample_rate: int | None = None
    channels: int | None = None
    layout: str | None = None
    width: int | None = None
    height: int | None = None
    framerate: Rational | None = None
    language: str | None = None
    metadata: dict[str, str] = field(default_factory=dict)
    color_raw: dict[str, str] = field(default_factory=dict)
    packets: list[PacketMeta] = field(default_factory=list)
    keyframes: list[Keyframe] = field(default_factory=list)
    #: Container-level sync-sample numbers (MP4 ``stss``); empty when absent.
    sync_samples: list[int] = field(default_factory=list)

    @property
    def packet_count(self) -> int:
        return len(self.packets)

    def to_dict(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "kind": self.kind,
            "codec": self.codec,
            "time_base": str(self.time_base),
            "start_raw": self.start_raw,
            "duration_raw": self.duration_raw,
            "sample_rate": self.sample_rate,
            "channels": self.channels,
            "layout": self.layout,
            "width": self.width,
            "height": self.height,
            "framerate": None if self.framerate is None else str(self.framerate),
            "language": self.language,
            "metadata": dict(self.metadata),
            "color_raw": dict(self.color_raw),
            "packet_count": self.packet_count,
            "packets": [p.to_dict() for p in self.packets],
            "keyframes": [k.to_dict() for k in self.keyframes],
            "sync_samples": list(self.sync_samples),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "StreamProbe":
        """Rebuild a probe record; packets are restored without payloads."""
        return cls(
            index=int(data["index"]),
            kind=str(data["kind"]),
            codec=str(data["codec"]),
            time_base=Rational.parse(data["time_base"]),
            start_raw=data.get("start_raw"),
            duration_raw=data.get("duration_raw"),
            sample_rate=data.get("sample_rate"),
            channels=data.get("channels"),
            layout=data.get("layout"),
            width=data.get("width"),
            height=data.get("height"),
            framerate=None if data.get("framerate") is None else Rational.parse(data["framerate"]),
            language=data.get("language"),
            metadata={str(k): str(v) for k, v in (data.get("metadata") or {}).items()},
            color_raw={str(k): str(v) for k, v in (data.get("color_raw") or {}).items()},
            packets=[PacketMeta.from_dict(p) for p in data.get("packets", [])],
            keyframes=[Keyframe.from_dict(k) for k in data.get("keyframes", [])],
            sync_samples=[int(x) for x in data.get("sync_samples", [])],
        )


@dataclass(slots=True)
class MappingRecord:
    """How stream roles were bound, preserved verbatim in the manifest."""

    basis: str
    video: int | None
    audio: list[int]
    requested: list[str] = field(default_factory=list)

    def role_of(self, index: int) -> str | None:
        if index == self.video:
            return "video"
        if index in self.audio:
            return "audio"
        return None

    def to_dict(self) -> dict[str, Any]:
        return {
            "basis": self.basis,
            "video": self.video,
            "audio": list(self.audio),
            "requested": list(self.requested),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "MappingRecord":
        return cls(
            basis=str(data["basis"]),
            video=None if data.get("video") is None else int(data["video"]),
            audio=[int(i) for i in data.get("audio", [])],
            requested=[str(s) for s in data.get("requested", [])],
        )


@dataclass(slots=True)
class ProbeResult:
    path: str
    size: int
    format_name: str
    duration_us: int | None
    time_base: TimeBase
    streams: list[StreamProbe]
    mapping: MappingRecord
    source_sha256: str = ""
    errors: list[MediaError] = field(default_factory=list)
    logs: list[str] = field(default_factory=list)

    def stream(self, index: int) -> StreamProbe | None:
        return next((s for s in self.streams if s.index == index), None)

    @property
    def video(self) -> StreamProbe | None:
        return None if self.mapping.video is None else self.stream(self.mapping.video)

    @property
    def audio(self) -> StreamProbe | None:
        return self.stream(self.mapping.audio[0]) if self.mapping.audio else None

    def stream_errors(self, index: int) -> list[MediaError]:
        return [e for e in self.errors if e.stream_index == index]

    def to_dict(self) -> dict[str, Any]:
        return {
            "path": self.path,
            "size": self.size,
            "format_name": self.format_name,
            "duration_us": self.duration_us,
            "time_base": str(self.time_base.rate),
            "source_sha256": self.source_sha256,
            "mapping": self.mapping.to_dict(),
            "streams": [s.to_dict() for s in self.streams],
            "errors": [e.to_dict() for e in self.errors],
            "logs": list(self.logs),
        }

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ProbeResult":
        return cls(
            path=str(data["path"]),
            size=int(data["size"]),
            format_name=str(data["format_name"]),
            duration_us=data.get("duration_us"),
            time_base=TimeBase(Rational.parse(data["time_base"])),
            streams=[StreamProbe.from_dict(s) for s in data.get("streams", [])],
            mapping=MappingRecord.from_dict(data["mapping"]),
            source_sha256=str(data.get("source_sha256", "")),
            errors=[MediaError.from_dict(e) for e in data.get("errors", [])],
            logs=[str(x) for x in data.get("logs", [])],
        )


def _attach_container_color(
    path: str, format_name: str, streams: list[StreamProbe]
) -> None:
    """Merge MOV/MP4 ``colr`` box values that PyAV's codec view omits.

    Values discovered here are recorded under ``colr:<field>`` so the colour
    resolver can cite them separately from codec and tag evidence.
    """
    from .mp4color import container_color

    short = {part.strip() for part in format_name.split(",")}
    fmt = "mov" if "mov" in short else "mp4" if ("mp4" in short or "m4a" in short) else ""
    if not fmt:
        return
    found = container_color(path, fmt)
    video_streams = [s for s in streams if s.kind == "video"]
    for position, values in found.items():
        if position >= len(video_streams):
            continue
        target = video_streams[position]
        for field_name, value in values.items():
            target.color_raw[f"colr:{field_name}"] = str(value)


def _attach_sample_index(
    path: str, format_name: str, streams: list[StreamProbe]
) -> None:
    """Record the MP4 ``stss`` sync-sample index alongside demuxed keyframes.

    Demuxers usually trust the bitstream over the index, so a ``stss`` that
    omits a real keyframe is invisible unless the container index is read
    separately.  Both are kept so verification can compare them.
    """
    from .mp4color import read_mp4_keyframe_index

    short = {part.strip() for part in format_name.split(",")}
    if not ({"mp4", "mov", "m4a"} & short):
        return
    indices = read_mp4_keyframe_index(path)
    video_streams = [s for s in streams if s.kind == "video"]
    for position, samples in enumerate(indices):
        if position >= len(video_streams):
            break
        video_streams[position].sync_samples = list(samples)


def _format_label(format_name: str) -> str:
    parts = {part.strip() for part in format_name.split(",")}
    for short in ("mov", "mp4", "matroska", "webm", "m4a"):
        if short in parts:
            return FORMAT_LABEL[short]
    return format_name


def parse_mapping_spec(specs: Iterable[str]) -> list[tuple[str, int]]:
    """Parse ``video:0`` / ``a:1`` style specs; raises ValueError for bad text."""
    parsed: list[tuple[str, int]] = []
    for spec in specs:
        if ":" not in spec:
            raise ValueError(f"mapping must look like 'video:0', got {spec!r}")
        role, _, raw = spec.partition(":")
        role = role.strip().lower()
        if role in ("v", "video"):
            role = "video"
        elif role in ("a", "audio"):
            role = "audio"
        else:
            raise ValueError(f"unknown mapping role {role!r} in {spec!r}")
        if not raw.strip().lstrip("-").isdigit():
            raise ValueError(f"stream index must be an integer in {spec!r}")
        index = int(raw)
        if index < 0:
            raise ValueError(f"stream index must be non-negative in {spec!r}")
        parsed.append((role, index))
    return parsed


def _resolve_mapping(
    streams: list[StreamProbe], specs: Iterable[str], path: str
) -> tuple[MappingRecord, list[MediaError]]:
    """Bind roles explicitly, or auto-bind the first video plus first audio."""
    by_index = {s.index: s for s in streams}
    errors: list[MediaError] = []
    requested = list(specs)
    if not requested:
        video = next((s.index for s in streams if s.kind == "video"), None)
        audio = [next((s.index for s in streams if s.kind == "audio"), -1)]
        audio = [i for i in audio if i >= 0]
        return MappingRecord("auto-first", video, audio, []), errors

    video: int | None = None
    audio: list[int] = []
    seen: dict[int, str] = {}
    for role, index in parse_mapping_spec(requested):
        stream = by_index.get(index)
        if stream is None:
            errors.append(
                MediaError(
                    "stream-index-out-of-range",
                    f"mapping {role}:{index} names a stream the container does not have",
                    stage="probe", path=path, stream_index=index,
                )
            )
            continue
        if stream.kind != role:
            errors.append(
                MediaError(
                    "type-mismatch",
                    f"mapping {role}:{index} targets a {stream.kind} stream",
                    stage="probe", path=path, stream_index=index,
                )
            )
            continue
        if index in seen:
            errors.append(
                MediaError(
                    "duplicate-mapping",
                    f"stream {index} is mapped both as {seen[index]} and {role}",
                    stage="probe", path=path, stream_index=index,
                )
            )
            continue
        seen[index] = role
        if role == "video":
            if video is not None:
                errors.append(
                    MediaError(
                        "duplicate-mapping",
                        f"video role already bound to stream {video}, not {index}",
                        stage="probe", path=path, stream_index=index,
                    )
                )
                continue
            video = index
        else:
            audio.append(index)
    return MappingRecord("explicit", video, audio, requested), errors


def _color_fields(stream: av.stream.Stream) -> dict[str, str]:
    """Raw colour values exactly as the container/codec reports them.

    Values are kept as strings of the FFmpeg enumerations; interpretation (and
    conflict resolution) belongs to :mod:`mediarc.color`.  ``UNSPECIFIED``
    entries are omitted rather than normalised, so their absence stays visible.
    """
    cc = stream.codec_context
    raw: dict[str, str] = {}
    for ours, theirs, unspecified in (
        ("primaries", "color_primaries", 2),
        ("transfer", "color_trc", 2),
        ("space", "colorspace", 2),
        ("range", "color_range", 0),
    ):
        value = getattr(cc, theirs, None)
        if value is None or int(value) == unspecified:
            continue
        raw[ours] = str(int(value))
    for key, value in dict(stream.metadata).items():
        upper = key.upper()
        if upper in ("COLOR_PRIMARIES", "COLOR_TRANSFER", "COLOR_SPACE", "COLOR_RANGE"):
            raw[f"tag:{upper}"] = str(value)
    return raw


def _stream_probe(stream: av.stream.Stream) -> StreamProbe:
    cc = stream.codec_context
    tb = stream.time_base
    probe = StreamProbe(
        index=stream.index,
        kind=stream.type,
        codec=cc.name,
        time_base=Rational(tb.numerator, tb.denominator) if tb else Rational(1, 1),
        start_raw=stream.start_time,
        duration_raw=stream.duration,
        language=stream.language,
        metadata={str(k): str(v) for k, v in dict(stream.metadata).items()},
    )
    if stream.type == "video":
        probe.width = cc.width
        probe.height = cc.height
        rate = stream.average_rate or stream.guessed_rate or cc.framerate
        if rate:
            probe.framerate = Rational(rate.numerator, rate.denominator)
        probe.color_raw = _color_fields(stream)
    elif stream.type == "audio":
        probe.sample_rate = cc.sample_rate
        probe.channels = cc.channels
        probe.layout = cc.layout.name if cc.layout is not None else None
    return probe


def probe(
    path: str,
    specs: Iterable[str] = (),
    *,
    limits: dict[str, Any] | None = None,
) -> ProbeResult:
    """Open ``path``, bind streams, and collect packets, keyframes and defects."""
    limits = limits or {}
    max_bytes = int(limits.get("max_file_bytes", 0))
    try:
        size = os.path.getsize(path)
    except OSError as exc:
        raise MediaError("io", str(exc), stage="probe", path=path) from exc
    if max_bytes and size > max_bytes:
        raise MediaError(
            "limit-exceeded",
            f"file is {size} bytes, limit is {max_bytes}",
            stage="probe", path=path,
        )
    try:
        source_sha = sha256_file(path)
    except OSError as exc:
        raise MediaError("io", str(exc), stage="probe", path=path) from exc

    logs: list[str] = []
    container = None
    format_name = ""
    streams: list[StreamProbe] = []
    try:
        with av.logging.Capture() as capture:
            av.logging.set_level(av.logging.WARNING)
            try:
                container = av.open(path)
                format_name = container.format.name if container.format else ""
                short = {part.strip() for part in format_name.split(",")}
                if not (short & ALLOWED_FORMATS):
                    raise MediaError(
                        "unsupported-container",
                        f"format {format_name!r} is not MOV/MP4/MKV",
                        stage="probe", path=path,
                    )
                streams = [_stream_probe(s) for s in container.streams]
                mapping, mapping_errors = _resolve_mapping(streams, specs, path)
                rate = choose_timeline_rate(
                    [s.time_base for s in streams if s.kind == "video"],
                    [s.sample_rate for s in streams if s.kind == "audio" and s.sample_rate],
                )
                timeline = TimeBase(rate)
                errors: list[MediaError] = list(mapping_errors)
                _attach_container_color(path, format_name, streams)
                _attach_sample_index(path, format_name, streams)
                observed_end = _demux_container(
                    container, streams, timeline, errors, path
                )
                duration_us = container.duration
                errors.extend(
                    _completeness(path, streams, observed_end, timeline)
                )
                logs = [line[2].strip() for line in capture]
            finally:
                # ``Capture.__exit__`` is not reached through this path, so the
                # level is restored here; the callback itself is restored by
                # whichever guard (or default) is active when probing starts.
                av.logging.set_level(av.logging.WARNING)
    except MediaError:
        raise
    except Exception as exc:  # noqa: BLE001 - libavformat raises many types
        raise bridge(
            "probe-failed", exc, stage="probe", path=path,
            detail={"logs": logs[-3:]},
        ) from exc
    finally:
        if container is not None:
            try:
                container.close()
            except Exception:  # noqa: BLE001
                pass

    if not streams:
        raise MediaError("probe-failed", "container has no streams", stage="probe", path=path)
    # An explicit mapping is authoritative: a bad spec is fatal for the run and
    # must name the source path and stream number rather than silently falling
    # back to some other stream.
    mapping_error = next(
        (e for e in mapping_errors if e.code in {
            "stream-index-out-of-range", "type-mismatch", "duplicate-mapping"
        }),
        None,
    )
    if mapping_error is not None:
        raise mapping_error
    if mapping.video is None and not mapping.audio:
        raise MediaError(
            "missing-stream",
            "no usable audio or video stream is available for archiving",
            stage="probe", path=path,
        )
    return ProbeResult(
        path=path,
        size=size,
        format_name=_format_label(format_name),
        duration_us=duration_us,
        time_base=timeline,
        streams=streams,
        mapping=mapping,
        source_sha256=source_sha,
        errors=errors,
        logs=logs,
    )


def _demux_container(
    container: av.container.InputContainer,
    streams: list[StreamProbe],
    timeline: TimeBase,
    errors: list[MediaError],
    path: str,
) -> dict[int, Fraction]:
    """Single streaming demux pass over every stream.

    libavformat interleaves reads, so demuxing stream-by-stream on one
    container truncates everything after the first stream.  One pass costs the
    same and preserves each packet's true source offset.
    """
    by_index = {s.index: s for s in streams}
    entries: dict[int, list[tuple[int, int | None, int | None, int | None]]] = {
        s.index: [] for s in streams
    }
    end: dict[int, Fraction] = {s.index: Fraction(0) for s in streams}
    counters: dict[int, int] = {s.index: 0 for s in streams}
    demuxer = container.demux()
    while True:
        try:
            packet = next(demuxer)
        except StopIteration:
            break
        except Exception as exc:  # noqa: BLE001 - libavformat error types vary
            last = max(
                (p.offset or 0 for s in streams for p in s.packets), default=None
            )
            errors.append(
                bridge(
                    "corrupt-packet", exc, stage="probe", path=path,
                    offset=last,
                )
            )
            break
        if packet is None:
            continue
        # libavformat emits zero-size flush packets at EOF; they carry neither
        # payload nor timing and would otherwise inflate the keyframe/packet
        # index with entries that have no source offset.
        if packet.size == 0 and packet.pts is None and packet.dts is None:
            continue
        stream = by_index.get(packet.stream_index)
        if stream is None:
            continue
        offset = packet.pos if packet.pos is not None and packet.pos >= 0 else None
        meta = PacketMeta(
            index=counters[stream.index],
            pts=packet.pts,
            dts=packet.dts,
            duration=packet.duration,
            size=packet.size,
            offset=offset,
            keyframe=bool(packet.is_keyframe),
            corrupt=bool(packet.is_corrupt),
        )
        counters[stream.index] += 1
        stream.packets.append(meta)
        entries[stream.index].append((meta.index, meta.pts, meta.dts, meta.offset))
        if meta.corrupt:
            errors.append(
                MediaError(
                    "corrupt-packet",
                    f"packet {meta.index} is flagged corrupt",
                    stage="probe", path=path, stream_index=stream.index,
                    offset=meta.offset,
                    detail={"packet_index": meta.index},
                )
            )
        if meta.keyframe and meta.pts is not None:
            stream.keyframes.append(
                Keyframe(
                    ticks=timeline.to_ticks(Fraction(meta.pts) * stream.time_base.value),
                    offset=meta.offset,
                    size=meta.size,
                    pts=meta.pts,
                )
            )
        if meta.pts is not None:
            ticks = Fraction(meta.pts) * stream.time_base.value
            span = Fraction(meta.duration or 0) * stream.time_base.value
            end[stream.index] = max(end[stream.index], ticks + span)
    for stream in streams:
        issues = analyze_timestamps(
            stream.index, entries[stream.index], stream.time_base, timeline
        )
        errors.extend(issue.error(path) for issue in issues)
    return end


def _declared_seconds(stream: StreamProbe) -> Fraction | None:
    """Duration this stream promises, from its own header (not the container)."""
    if stream.duration_raw is not None:
        return Fraction(stream.duration_raw) * stream.time_base.value
    tag = stream.metadata.get("DURATION") or stream.metadata.get("duration")
    if tag:
        try:
            hours, minutes, seconds = tag.split(":")
            return Fraction(hours) * 3600 + Fraction(minutes) * 60 + Fraction(seconds)
        except (ValueError, ZeroDivisionError):
            return None
    return None


def _completeness(
    path: str,
    streams: list[StreamProbe],
    observed_end: dict[int, Fraction],
    timeline: TimeBase,
) -> list[MediaError]:
    """Flag streams whose packets stop well short of what their header declares.

    Per-stream declarations are used because a container duration describes the
    longest track, not each track: a deliberately short audio track is not a
    truncated file.  A 250 ms tolerance absorbs frame rounding.
    """
    errors: list[MediaError] = []
    for stream in streams:
        if not stream.packets:
            continue  # an empty track is reported by the timeline stage instead
        declared = _declared_seconds(stream)
        if declared is None:
            continue
        end = observed_end.get(stream.index, Fraction(0))
        if declared - end > Fraction(1, 4):
            last = stream.packets[-1]
            errors.append(
                MediaError(
                    "truncated-container",
                    f"stream {stream.index} ends at {float(end):.3f}s but its header "
                    f"declares {float(declared):.3f}s",
                    stage="probe", path=path, stream_index=stream.index,
                    offset=last.offset,
                    detail={
                        "declared_seconds": round(float(declared), 3),
                        "observed_seconds": round(float(end), 3),
                    },
                )
            )
    return errors


def keyframe_ticks(probe_stream: StreamProbe) -> list[int]:
    return [k.ticks for k in probe_stream.keyframes]


def packet_at(probe_stream: StreamProbe, ticks: int, timeline: TimeBase) -> PacketMeta | None:
    """First packet whose presentation tick is >= ``ticks``."""
    for meta in probe_stream.packets:
        if meta.pts is None:
            continue
        if timeline.to_ticks(Fraction(meta.pts) * probe_stream.time_base.value) >= ticks:
            return meta
    return None


def safe_boundary(probe_stream: StreamProbe, ticks: int) -> Keyframe | None:
    """Latest keyframe at or before ``ticks`` (the repair restart point)."""
    found = None
    for key in probe_stream.keyframes:
        if key.ticks <= ticks:
            found = key
        else:
            break
    return found


def probe_summary(result: ProbeResult) -> dict[str, Any]:
    return {
        "tool": f"mediarc/{__version__}",
        "path": result.path,
        "size": result.size,
        "format": result.format_name,
        "source_sha256": result.source_sha256,
    }
