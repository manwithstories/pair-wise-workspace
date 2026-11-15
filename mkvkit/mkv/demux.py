"""Matroska demux: Segment/Tracks/Cluster/BlockGroup parsing.

Produces a frame model (decode order, presentation timestamps in
nanoseconds) shared by the remux pipeline.  SimpleBlock and
BlockGroup>Block are supported, with Xiph / EBML / fixed-size lacing.
"""
from __future__ import annotations

import struct
from dataclasses import dataclass, field

from ..ebml import schema
from ..ebml.reader import EbmlError, EbmlReader, NotMatroskaError, TruncatedError
from ..ebml.vint import read_vint, decode_signed, VintError

CODEC_MAP = {
    "V_MPEG4/ISO/AVC": ("avc1", "video"),
    "A_AAC": ("mp4a", "audio"),
    "A_AAC/MPEG4/LC": ("mp4a", "audio"),
    "A_AAC/MPEG4/LC/SBR": ("mp4a", "audio"),
    "S_TEXT/UTF8": ("tx3g", "subtitle"),
}


class UnsupportedCodecError(Exception):
    """CodecID cannot be mapped or its private data is missing (exit 3)."""


@dataclass
class TrackInfo:
    number: int
    kind: str
    codec_id: str
    mp4_codec: str
    codec_private: bytes | None
    language: str = "und"
    width: int = 0
    height: int = 0
    channels: int = 0
    sample_rate: int = 0
    default_duration_ns: int = 0


@dataclass
class Frame:
    track_number: int
    pts_ns: int
    offset: int
    size: int
    keyframe: bool
    cluster_index: int


@dataclass
class DemuxResult:
    tracks: list[TrackInfo]
    frames: dict[int, list[Frame]]
    timestamp_scale: int
    issues: list[str] = field(default_factory=list)


def _uint(reader, el, default=0):
    if el is None:
        return default
    return int.from_bytes(reader.read_payload(el), "big")


def _text(reader, el, default=""):
    if el is None:
        return default
    return reader.read_payload(el).decode("utf-8", "replace").rstrip("\x00")


def _float(reader, el, default=0.0):
    if el is None:
        return default
    data = reader.read_payload(el)
    if len(data) == 4:
        return struct.unpack(">f", data)[0]
    if len(data) == 8:
        return struct.unpack(">d", data)[0]
    raise EbmlError(f"invalid float payload at {el.offset}")


def _child(el, eid):
    for c in el.children:
        if c.id == eid:
            return c
    return None


def validate_matroska(reader, roots):
    """Ensure the file starts with an EBML header declaring matroska/webm."""
    if not roots or roots[0].id != schema.ID_EBML or roots[0].truncated:
        raise NotMatroskaError("missing EBML header")
    doctype_el = _child(roots[0], schema.ID_DOCTYPE)
    if doctype_el is None or doctype_el.truncated:
        raise NotMatroskaError("missing DocType")
    doctype = _text(reader, doctype_el)
    if doctype not in ("matroska", "webm"):
        raise NotMatroskaError(f"unsupported DocType {doctype!r}")
    return doctype


def _parse_tracks(reader, tracks_el) -> list[TrackInfo]:
    tracks = []
    for te in tracks_el.children:
        if te.id != schema.ID_TRACK_ENTRY:
            continue
        number = _uint(reader, _child(te, schema.ID_TRACK_NUMBER))
        codec_id = _text(reader, _child(te, schema.ID_CODEC_ID))
        mapping = CODEC_MAP.get(codec_id)
        if mapping is None and codec_id.startswith("A_AAC"):
            mapping = ("mp4a", "audio")
        if mapping is None:
            raise UnsupportedCodecError(
                f"track {number}: unsupported CodecID {codec_id!r}"
            )
        mp4_codec, kind = mapping
        private_el = _child(te, schema.ID_CODEC_PRIVATE)
        codec_private = (
            reader.read_payload(private_el)
            if private_el is not None and not private_el.truncated
            else None
        )
        if mp4_codec in ("avc1", "mp4a") and not codec_private:
            raise UnsupportedCodecError(
                f"track {number}: missing CodecPrivate for {mp4_codec}"
            )
        video = _child(te, schema.ID_VIDEO)
        audio = _child(te, schema.ID_AUDIO)
        channels = _uint(reader, _child(audio, schema.ID_CHANNELS)) if audio else 0
        sample_rate = (
            int(_float(reader, _child(audio, schema.ID_SAMPLING_FREQUENCY), 0.0))
            if audio else 0
        )
        default_duration_ns = _uint(
            reader, _child(te, schema.ID_DEFAULT_DURATION), 0
        )
        tracks.append(
            TrackInfo(
                number=number,
                kind=kind,
                codec_id=codec_id,
                mp4_codec=mp4_codec,
                codec_private=codec_private,
                language=_text(reader, _child(te, schema.ID_LANGUAGE), "und") or "und",
                width=_uint(reader, _child(video, schema.ID_PIXEL_WIDTH)) if video else 0,
                height=_uint(reader, _child(video, schema.ID_PIXEL_HEIGHT)) if video else 0,
                channels=channels or 2 if audio else 0,
                sample_rate=sample_rate or 48000 if audio else 0,
                default_duration_ns=default_duration_ns,
            )
        )
    return tracks


def _expand_lacing(data, pos, lacing, issues, where):
    """Return list of (offset_in_payload, size) for each laced frame."""
    num = data[pos] + 1
    if num < 2:
        raise ValueError("lacing header describes fewer than two frames")
    pos += 1
    sizes = []
    if lacing == 1:  # Xiph
        for _ in range(num - 1):
            total = 0
            while True:
                if pos >= len(data):
                    raise ValueError("xiph lacing header overrun")
                total += data[pos]
                pos += 1
                if data[pos - 1] != 255:
                    break
            sizes.append(total)
    elif lacing == 2:  # fixed-size
        rest = len(data) - pos
        if num == 0 or rest % num:
            raise ValueError("fixed lacing size mismatch")
        sizes = [rest // num] * (num - 1)
    else:  # EBML lacing
        first, length, _ = read_vint(data, pos)
        pos += length
        sizes.append(first)
        for _ in range(num - 2):
            delta, length, _ = read_vint(data, pos)
            pos += length
            sizes.append(sizes[-1] + decode_signed(delta, length))
    remaining = len(data) - pos - sum(sizes)
    sizes.append(remaining)
    if remaining < 0 or any(s < 0 for s in sizes):
        raise ValueError("lacing sizes exceed block payload")
    out = []
    for size in sizes:
        out.append((pos, size))
        pos += size
    return out


def _laced_delta_ns(track, block_duration, timestamp_scale, index, count):
    """Presentation offset of frame *index* inside a laced block."""
    if block_duration:
        return index * block_duration * timestamp_scale // count
    if track is not None and track.default_duration_ns:
        return index * track.default_duration_ns
    if track is not None and track.kind == "audio" and track.sample_rate:
        # AAC-LC uses 1024 samples per access unit.  This fallback only
        # applies when a muxer omitted BlockDuration/DefaultDuration.
        return index * 1024 * 1_000_000_000 // track.sample_rate
    return index * timestamp_scale


def _parse_block(reader, el, keyframe, block_duration, cluster_ts,
                 cluster_index, frames, issues, timestamp_scale, by_number):
    try:
        data = reader.read_payload(el)
        track_number, tlen, _ = read_vint(data, 0)
        ts = struct.unpack(">h", data[tlen:tlen + 2])[0]
        flags = data[tlen + 2]
        pos = tlen + 3
        lacing = (flags >> 1) & 0x03
        if keyframe is None:
            keyframe = bool(flags & 0x80)
        if lacing == 0:
            spans = [(pos, len(data) - pos)]
        else:
            spans = _expand_lacing(data, pos, lacing, issues, el.offset)
    except (ValueError, IndexError, struct.error, TruncatedError, VintError) as exc:
        issues.append(f"corrupt block at {el.offset} skipped: {exc}")
        return
    track = by_number.get(track_number)
    if track is None:
        issues.append(f"unknown track {track_number} at {el.offset} skipped")
        return
    count = len(spans)
    base_ns = (cluster_ts + ts) * timestamp_scale
    for idx, (off, size) in enumerate(spans):
        if size < 0 or off < 0 or off + size > len(data):
            issues.append(f"corrupt laced frame at {el.offset} skipped")
            continue
        pts_ns = base_ns + _laced_delta_ns(
            track, block_duration, timestamp_scale, idx, count
        ) if count > 1 else base_ns
        frames.setdefault(track_number, []).append(
            Frame(track_number, pts_ns, el.data_offset + off, size,
                  keyframe, cluster_index)
        )


def demux(path) -> DemuxResult:
    issues: list[str] = []
    with EbmlReader(path, tolerant=True) as reader:
        roots = reader.parse()
        validate_matroska(reader, roots)
        segment = next((e for e in roots if e.id == schema.ID_SEGMENT), None)
        if segment is None:
            raise NotMatroskaError("no Segment element found")
        timestamp_scale = 1_000_000
        tracks: list[TrackInfo] = []
        for child in segment.children:
            if child.id == schema.ID_INFO:
                timestamp_scale = _uint(
                    reader, _child(child, schema.ID_TIMESTAMP_SCALE), 1_000_000
                )
            elif child.id == schema.ID_TRACKS:
                tracks = _parse_tracks(reader, child)
        if not tracks:
            raise NotMatroskaError("no Tracks element found")
        by_number = {track.number: track for track in tracks}

        frames: dict[int, list[Frame]] = {}
        cluster_index = 0
        for child in segment.children:
            if child.id == schema.ID_CLUSTER:
                ts_el = _child(child, schema.ID_TIMESTAMP)
                cluster_ts = _uint(reader, ts_el) if ts_el else 0
                for node in child.children:
                    if node.truncated:
                        continue  # counted via reader.truncations below
                    if node.id == schema.ID_SIMPLE_BLOCK:
                        _parse_block(reader, node, None, 0, cluster_ts,
                                     cluster_index, frames, issues,
                                     timestamp_scale, by_number)
                    elif node.id == schema.ID_BLOCK_GROUP:
                        block_el = _child(node, schema.ID_BLOCK)
                        if block_el is None or block_el.truncated:
                            continue
                        keyframe = _child(node, schema.ID_REFERENCE_BLOCK) is None
                        duration = _uint(
                            reader, _child(node, schema.ID_BLOCK_DURATION), 0
                        )
                        _parse_block(reader, block_el, keyframe, duration,
                                     cluster_ts, cluster_index, frames, issues,
                                     timestamp_scale, by_number)
                cluster_index += 1
        for off in reader.truncations:
            issues.append(f"truncated element at offset {off} skipped")
    known = {t.number for t in tracks}
    frames = {n: f for n, f in frames.items() if n in known}
    return DemuxResult(tracks, frames, timestamp_scale, issues)
