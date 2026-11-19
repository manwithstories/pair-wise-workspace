"""Container inspection: streams, copyable descriptors and packet events.

The demuxer in this module is deliberately read-only.  It never decodes media
for the remux path and never writes to the input; it produces

* :class:`StreamInfo` -- the track inventory used by the track mapper and the
  report (index, codec parameters, time base, exact first/last stamps).
* :class:`RawPacket` -- the compressed payload plus its container coordinates,
  which the remux stage writes back byte-for-byte.

MP4/MOV structural inspection lives in :mod:`bcastqc.container` so the muxer
and the recovery scanner can share one box parser.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Iterator

import av

from .errors import (
    InputError,
    LosslessCopyRefusedError,
    MediaLocation,
    QCError,
    UnrecoverableMediaError,
)
from .timeline import PacketSpan, seconds

#: Video codecs that every MP4/MOV delivery in this pipeline may carry.  These
#: are the codecs a broadcast supplier realistically delivers and that the
#: ISO-BMFF / QuickTime containers accept without re-encoding.
LOSSLESS_VIDEO_CODECS = frozenset({"h264", "hevc", "mpeg4", "mpeg2video", "mjpeg", "prores"})
LOSSLESS_AUDIO_CODECS = frozenset({
    "aac",
    "pcm_s16le",
    "pcm_s24le",
    "pcm_s32le",
    "pcm_f32le",
    "pcm_s16be",
    "pcm_s24be",
    "pcm_s32be",
    "alac",
    "ac3",
    "eac3",
    "mp2",
})

#: Container families we accept.  Anything else is a usage error, not a media
#: error: the operator pointed the tool at the wrong file.
ALLOWED_EXTENSIONS = {".mp4", ".mov", ".m4v", ".m4a"}

MAX_ASSET_BYTES = 2 * 1024 * 1024 * 1024  # 2 GiB
MAX_TRACKS = 64


@dataclass(slots=True)
class StreamInfo:
    index: int
    type: str
    codec: str
    codec_long_name: str
    time_base: Fraction
    time_base_raw: Fraction
    stream_id: int | None
    codec_tag: str | None
    extradata: bytes
    extradata_sha256: str
    start_time: Fraction | None
    declared_duration: Fraction | None
    # Codec parameters that must match for a lossless copy to stay valid.
    width: int | None = None
    height: int | None = None
    pix_fmt: str | None = None
    frame_rate: Fraction | None = None
    sample_rate: int | None = None
    channels: int | None = None
    sample_format: str | None = None
    bit_rate: int | None = None
    language: str | None = None
    # Filled while packet scanning.
    packet_count: int = 0
    first_pts: Fraction | None = None
    last_end: Fraction | None = None
    timeline: StreamTimeline | None = None

    @property
    def kind(self) -> str:
        return self.type

    @property
    def copyable(self) -> bool:
        if self.type == "video":
            return self.codec in LOSSLESS_VIDEO_CODECS
        if self.type == "audio":
            return self.codec in LOSSLESS_AUDIO_CODECS
        return False

    def to_dict(self) -> dict[str, object]:
        out: dict[str, object] = {
            "index": self.index,
            "type": self.type,
            "codec": self.codec,
            "codec_long_name": self.codec_long_name,
            "time_base": f"{self.time_base.numerator}/{self.time_base.denominator}",
            "time_base_float": round(float(self.time_base), 9),
            "stream_id": self.stream_id,
            "codec_tag": self.codec_tag,
            "extradata_sha256": self.extradata_sha256,
            "extradata_size": len(self.extradata),
            "declared_start_time": _s(self.start_time),
            "declared_duration": _s(self.declared_duration),
            "packets": self.packet_count,
            "first_pts": _s(self.first_pts),
            "last_end": _s(self.last_end),
            "copyable": self.copyable,
        }
        for key in (
            "width",
            "height",
            "pix_fmt",
            "sample_rate",
            "channels",
            "sample_format",
            "bit_rate",
            "language",
        ):
            value = getattr(self, key)
            if value is not None:
                out[key] = value
        if self.frame_rate is not None:
            out["frame_rate"] = f"{self.frame_rate.numerator}/{self.frame_rate.denominator}"
        return out


@dataclass(slots=True)
class RawPacket:
    """One packet as read from the container, before any mux-time change."""

    stream: int
    pts: int | None
    dts: int | None
    duration: int | None
    pos: int | None
    size: int
    keyframe: bool
    data: bytes
    time_base: Fraction
    corrupt: bool = False

    def to_span(self) -> PacketSpan:
        return PacketSpan(
            stream=self.stream,
            pts=seconds(self.pts, self.time_base),
            dts=seconds(self.dts, self.time_base),
            duration=seconds(self.duration, self.time_base),
            pos=self.pos,
            size=self.size,
            keyframe=self.keyframe,
            raw_pts=self.pts,
            raw_dts=self.dts,
            raw_duration=self.duration,
        )

    def sha256(self) -> str:
        return hashlib.sha256(self.data).hexdigest()


@dataclass(slots=True)
class AssetProbe:
    path: Path
    format_names: tuple[str, ...]
    size: int
    duration: Fraction | None
    streams: list[StreamInfo]
    warnings: list[QCError] = field(default_factory=list)

    def by_index(self, index: int) -> StreamInfo | None:
        for stream in self.streams:
            if stream.index == index:
                return stream
        return None

    def to_dict(self) -> dict[str, object]:
        return {
            "path": str(self.path),
            "format": list(self.format_names),
            "size": self.size,
            "duration": _s(self.duration),
            "streams": [stream.to_dict() for stream in self.streams],
        }


def _s(value: Fraction | None) -> float | None:
    return None if value is None else round(float(value), 6)


def _time_base(stream: av.stream.Stream) -> tuple[Fraction, Fraction]:
    """Return (media time base, container time base) as exact fractions."""
    raw = stream.time_base
    media = Fraction(1, 1)
    cc = stream.codec_context
    if stream.type == "audio" and cc.sample_rate:
        media = Fraction(1, int(cc.sample_rate))
    elif stream.type == "video" and stream.average_rate:
        rate = Fraction(stream.average_rate)
        if rate > 0:
            media = 1 / rate
    if raw is None:
        return media, media
    raw_frac = Fraction(raw.numerator, raw.denominator)
    # PyAV may hand us a guessed time base; fall back when it is clearly off.
    if raw_frac <= 0:
        return media, media
    return media, raw_frac


def validate_asset_path(path: Path, *, max_bytes: int = MAX_ASSET_BYTES) -> None:
    if not path.exists():
        raise InputError(f"input does not exist: {path}", location=MediaLocation(asset=str(path)))
    if not path.is_file():
        raise InputError(f"input is not a regular file: {path}", location=MediaLocation(asset=str(path)))
    suffix = path.suffix.lower()
    if suffix not in ALLOWED_EXTENSIONS:
        raise InputError(
            f"unsupported extension {suffix!r}; expected one of {sorted(ALLOWED_EXTENSIONS)}",
            location=MediaLocation(asset=str(path)),
        )
    size = path.stat().st_size
    if size == 0:
        raise InputError("input is empty", location=MediaLocation(asset=str(path)))
    if size > max_bytes:
        raise InputError(
            f"input is {size} bytes, above the {max_bytes} byte limit",
            location=MediaLocation(asset=str(path)),
            code="input_too_large",
        )


def probe_asset(path: Path, *, max_tracks: int = MAX_TRACKS) -> AssetProbe:
    """Open a file and describe every stream without decoding anything."""
    validate_asset_path(path)
    try:
        container = av.open(str(path), mode="r", metadata_errors="ignore")
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        raise UnrecoverableMediaError(
            f"cannot open container: {exc}", location=MediaLocation(asset=str(path))
        ) from exc
    try:
        if len(container.streams) > max_tracks:
            raise InputError(
                f"{len(container.streams)} tracks exceed the {max_tracks} track limit",
                location=MediaLocation(asset=str(path)),
                code="too_many_tracks",
            )
        streams: list[StreamInfo] = []
        for stream in container.streams:
            cc = stream.codec_context
            media_tb, raw_tb = _time_base(stream)
            extradata = bytes(cc.extradata or b"")
            start = seconds(stream.start_time, raw_tb) if stream.start_time is not None else None
            rate = None
            if stream.type == "video" and stream.average_rate:
                rate = Fraction(stream.average_rate)
            info = StreamInfo(
                index=stream.index,
                type=stream.type,
                codec=cc.name or "unknown",
                codec_long_name=getattr(cc, "codec", None).long_name if getattr(cc, "codec", None) else "",
                time_base=media_tb,
                time_base_raw=raw_tb,
                stream_id=getattr(stream, "id", None),
                codec_tag=_tag(cc),
                extradata=extradata,
                extradata_sha256=hashlib.sha256(extradata).hexdigest(),
                start_time=start,
                declared_duration=seconds(stream.duration, media_tb) if stream.duration else None,
                width=getattr(cc, "width", None),
                height=getattr(cc, "height", None),
                pix_fmt=getattr(cc, "pix_fmt", None),
                frame_rate=rate,
                sample_rate=getattr(cc, "sample_rate", None),
                channels=getattr(cc, "channels", None),
                sample_format=_fmt_name(getattr(cc, "format", None)),
                bit_rate=getattr(cc, "bit_rate", None),
                language=stream.metadata.get("language") if stream.metadata else None,
            )
            streams.append(info)
        duration = None
        if container.duration:
            duration = Fraction(container.duration, 1_000_000)
        names = tuple(container.format.name.split(",")) if container.format else ()
        return AssetProbe(
            path=path,
            format_names=names,
            size=path.stat().st_size,
            duration=duration,
            streams=streams,
        )
    finally:
        container.close()


def _fmt_name(value: object) -> str | None:
    if value is None:
        return None
    name = getattr(value, "name", None)
    return str(name) if name is not None else str(value)


def _tag(cc: object) -> str | None:
    tag = getattr(cc, "codec_tag", None)
    if tag is None:
        return None
    if isinstance(tag, str):
        return tag
    try:
        return int(tag).to_bytes(4, "little").decode("latin1").strip("\x00")
    except (TypeError, ValueError):
        return str(tag)


def iter_packets(
    path: Path,
    *,
    streams: list[int] | None = None,
    include_padding: bool = False,
    stop_after: int | None = None,
) -> Iterator[RawPacket]:
    """Yield packets in demux order without decoding.

    ``streams`` restricts the walk to the requested indices.  Empty padding
    packets (``size == 0``) are dropped unless explicitly requested because
    they carry no media and confuse byte accounting.
    """
    wanted = set(streams) if streams is not None else None
    try:
        container = av.open(str(path), mode="r", metadata_errors="ignore")
    except (av.error.FFmpegError, OSError, ValueError) as exc:
        raise UnrecoverableMediaError(
            f"cannot open container: {exc}", location=MediaLocation(asset=str(path))
        ) from exc
    try:
        time_bases = {
            stream.index: Fraction(stream.time_base.numerator, stream.time_base.denominator)
            for stream in container.streams
            if stream.time_base is not None
        }
        seen = 0
        for packet in container.demux():
            if packet.size == 0 and not include_padding:
                continue
            if packet.stream is None:
                continue
            index = packet.stream.index
            if wanted is not None and index not in wanted:
                continue
            tb = time_bases.get(index, Fraction(1, 1))
            yield RawPacket(
                stream=index,
                pts=packet.pts,
                dts=packet.dts,
                duration=packet.duration,
                pos=packet.pos,
                size=packet.size,
                keyframe=bool(packet.is_keyframe),
                data=bytes(packet),
                time_base=tb,
                corrupt=bool(packet.is_corrupt),
            )
            seen += 1
            if stop_after is not None and seen >= stop_after:
                return
    except av.error.FFmpegError as exc:
        raise UnrecoverableMediaError(
            f"demux failed: {exc}", location=MediaLocation(asset=str(path))
        ) from exc
    finally:
        container.close()


def check_lossless_support(streams: list[StreamInfo]) -> None:
    """Raise when a stream cannot be copied into MP4/MOV without re-encoding."""
    unsupported = [stream for stream in streams if not stream.copyable]
    if unsupported:
        details = ", ".join(
            f"stream {s.index} ({s.type}/{s.codec})" for s in unsupported
        )
        raise LosslessCopyRefusedError(
            f"cannot copy losslessly: {details}",
            location=MediaLocation(
                asset=None,
                stream=unsupported[0].index,
                detail=f"unsupported codec {unsupported[0].codec!r}",
            ),
        )


def format_guess(path: Path) -> str:
    suffix = path.suffix.lower()
    if suffix == ".mov":
        return "mov"
    return "mp4"
