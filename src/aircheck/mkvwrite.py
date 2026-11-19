"""Minimal deterministic Matroska writer used to synthesise damaged fixtures.

PyAV's muxer always produces a well-formed file, so it cannot express the
pathologies the pipeline must survive (wrapped timestamps, disordered blocks,
a Cues table pointing past EOF).  This writer emits just enough EBML to be
readable by FFmpeg while giving byte-level control over block timecodes and
cue positions.

It is a fixture tool: no seeking, no lacing, no CRC-32.  Every element is built
in memory and written once, so the result is fully reproducible.
"""

from __future__ import annotations

import struct
from dataclasses import dataclass, field
from pathlib import Path

# EBML element IDs used here.
_ID_EBML = b"\x1a\x45\xdf\xa3"
_ID_EBML_VERSION = b"\x42\x86"
_ID_EBML_READ_VERSION = b"\x42\xf7"
_ID_EBML_MAX_ID = b"\x42\xf2"
_ID_EBML_MAX_SIZE = b"\x42\xf3"
_ID_DOCTYPE = b"\x42\x82"
_ID_DOCTYPE_VERSION = b"\x42\x87"
_ID_DOCTYPE_READ_VERSION = b"\x42\x85"

_ID_SEGMENT = b"\x18\x53\x80\x67"
_ID_INFO = b"\x15\x49\xa9\x66"
_ID_TIMESTAMP_SCALE = b"\x2a\xd7\xb1"
_ID_MUXING_APP = b"\x4d\x80"
_ID_WRITING_APP = b"\x57\x41"
_ID_DURATION = b"\x44\x89"

_ID_TRACKS = b"\x16\x54\xae\x6b"
_ID_TRACK_ENTRY = b"\xae"
_ID_TRACK_NUMBER = b"\xd7"
_ID_TRACK_UID = b"\x73\xc5"
_ID_TRACK_TYPE = b"\x83"
_ID_CODEC_ID = b"\x86"
_ID_VIDEO = b"\xe0"
_ID_PIXEL_WIDTH = b"\xb0"
_ID_PIXEL_HEIGHT = b"\xba"
_ID_AUDIO = b"\xe1"
_ID_SAMPLING_FREQ = b"\xb5"
_ID_CHANNELS = b"\x9f"
_ID_BIT_DEPTH = b"\x62\x64"

_ID_CLUSTER = b"\x1f\x43\xb6\x75"
_ID_CLUSTER_TIMESTAMP = b"\xe7"
_ID_SIMPLE_BLOCK = b"\xa3"
_ID_CUES = b"\x1c\x53\xbb\x6b"
_ID_CUE_POINT = b"\xbb"
_ID_CUE_TIME = b"\xb3"
_ID_CUE_TRACK_POSITIONS = b"\xb7"
_ID_CUE_TRACK = b"\xf7"
_ID_CUE_CLUSTER_POSITION = b"\xf1"
_ID_CUE_RELATIVE_POSITION = b"\xf0"


def vint(value: int, length: int | None = None) -> bytes:
    """Encode an EBML variable-size integer with its length marker."""
    if value < 0:
        raise ValueError("vint must be non-negative")
    if length is None:
        for width in range(1, 9):
            if value < (1 << (7 * width)) - 1:
                length = width
                break
        else:
            raise ValueError("value too large for a vint")
    marker = 1 << (7 * length)
    return (value | marker).to_bytes(length, "big")


def element(element_id: bytes, payload: bytes) -> bytes:
    return element_id + vint(len(payload)) + payload


def uint_bytes(value: int) -> bytes:
    if value < 0:
        raise ValueError("unsigned element must be non-negative")
    width = max(1, (value.bit_length() + 7) // 8)
    return value.to_bytes(width, "big")


def float_bytes(value: float) -> bytes:
    return struct.pack(">d", value)


@dataclass(frozen=True)
class MkvTrack:
    number: int
    kind: str                       # "audio" | "video"
    codec_id: str = "A_PCM/INT/LIT"
    sample_rate: int = 48000
    channels: int = 2
    bit_depth: int = 16
    width: int = 64
    height: int = 48

    def encode(self) -> bytes:
        body = (
            element(_ID_TRACK_NUMBER, uint_bytes(self.number))
            + element(_ID_TRACK_UID, uint_bytes(self.number))
            + element(_ID_TRACK_TYPE, b"\x01" if self.kind == "video" else b"\x02")
            + element(_ID_CODEC_ID, self.codec_id.encode("ascii"))
        )
        if self.kind == "video":
            body += element(
                _ID_VIDEO,
                element(_ID_PIXEL_WIDTH, uint_bytes(self.width))
                + element(_ID_PIXEL_HEIGHT, uint_bytes(self.height)),
            )
        else:
            body += element(
                _ID_AUDIO,
                element(_ID_SAMPLING_FREQ, float_bytes(self.sample_rate))
                + element(_ID_CHANNELS, uint_bytes(self.channels))
                + element(_ID_BIT_DEPTH, uint_bytes(self.bit_depth)),
            )
        return element(_ID_TRACK_ENTRY, body)


@dataclass(frozen=True)
class MkvBlock:
    track: int
    timecode_ms: int
    keyframe: bool
    payload: bytes
    relative_ms: int | None = None   # set per cluster by the writer


@dataclass
class MkvCluster:
    timecode_ms: int
    blocks: list[MkvBlock] = field(default_factory=list)


@dataclass(frozen=True)
class MkvCue:
    track: int
    timecode_ms: int
    cluster_index: int
    cluster_position_override: int | None = None
    relative_position: int = 0


def _build_cluster(cluster: MkvCluster) -> bytes:
    body = bytearray(element(_ID_CLUSTER_TIMESTAMP, uint_bytes(cluster.timecode_ms)))
    for block in cluster.blocks:
        relative = block.timecode_ms - cluster.timecode_ms
        if not -32768 <= relative <= 32767:
            raise ValueError(
                f"block at {block.timecode_ms}ms is out of range for cluster at "
                f"{cluster.timecode_ms}ms"
            )
        flags = 0x80 if block.keyframe else 0x00
        body += element(
            _ID_SIMPLE_BLOCK,
            vint(block.track, 1) + struct.pack(">h", relative) + bytes([flags]) + block.payload,
        )
    return element(_ID_CLUSTER, bytes(body))


def _build_cues(
    cues: list[MkvCue], cluster_offsets: list[int], cluster_timecodes: list[int]
) -> bytes:
    if not cues:
        return b""
    points = bytearray()
    for cue in cues:
        points += _encode_cue(cue, _cue_position(cue, cluster_offsets))
    return element(_ID_CUES, bytes(points))


def _cue_position(cue: MkvCue, cluster_offsets: list[int]) -> int:
    if not 0 <= cue.cluster_index < len(cluster_offsets):
        raise ValueError(f"cue references missing cluster {cue.cluster_index}")
    if cue.cluster_position_override is not None:
        return cue.cluster_position_override
    return cluster_offsets[cue.cluster_index]


def _encode_cue(cue: MkvCue, cluster_position: int) -> bytes:
    """Encode a single CuePoint, letting the caller supply the position.

    Separating this from :func:`_build_cues` lets damaged fixtures write a Cues
    table whose order and offsets are deliberately wrong.
    """
    track_positions = element(
        _ID_CUE_TRACK_POSITIONS,
        element(_ID_CUE_TRACK, uint_bytes(cue.track))
        + element(_ID_CUE_CLUSTER_POSITION, uint_bytes(cluster_position))
        + element(_ID_CUE_RELATIVE_POSITION, uint_bytes(cue.relative_position)),
    )
    return element(
        _ID_CUE_POINT,
        element(_ID_CUE_TIME, uint_bytes(cue.timecode_ms)) + track_positions,
    )


def build_mkv(
    tracks: list[MkvTrack],
    clusters: list[MkvCluster],
    *,
    cues: list[MkvCue] | None = None,
    cues_blob: bytes | None = None,
    timestamp_scale_ns: int = 1_000_000,
    duration_ms: int | None = None,
    write_segment_size: bool = True,
    muxing_app: str = "aircheck-fixtures",
) -> bytes:
    """Serialise a complete MKV file and return its bytes.

    ``cues_blob`` overrides ``cues`` and must already contain the encoded
    contents of a ``Cues`` element; damaged fixtures use it to emit index
    tables that a well-behaved writer would never produce.
    """
    header = element(
        _ID_EBML,
        element(_ID_EBML_VERSION, b"\x01")
        + element(_ID_EBML_READ_VERSION, b"\x01")
        + element(_ID_EBML_MAX_ID, b"\x04")
        + element(_ID_EBML_MAX_SIZE, b"\x08")
        + element(_ID_DOCTYPE, b"matroska")
        + element(_ID_DOCTYPE_VERSION, b"\x04")
        + element(_ID_DOCTYPE_READ_VERSION, b"\x02"),
    )

    info_body = (
        element(_ID_TIMESTAMP_SCALE, uint_bytes(timestamp_scale_ns))
        + element(_ID_MUXING_APP, muxing_app.encode("ascii"))
        + element(_ID_WRITING_APP, muxing_app.encode("ascii"))
    )
    if duration_ms is not None:
        info_body += element(_ID_DURATION, float_bytes(float(duration_ms)))
    info = element(_ID_INFO, info_body)
    tracks_element = element(_ID_TRACKS, b"".join(track.encode() for track in tracks))

    prefix = info + tracks_element
    cluster_blobs: list[bytes] = []
    cluster_timecodes: list[int] = []
    offsets: list[int] = []
    cursor = len(prefix)
    for cluster in clusters:
        blob = _build_cluster(cluster)
        offsets.append(cursor)
        cluster_timecodes.append(cluster.timecode_ms)
        cluster_blobs.append(blob)
        cursor += len(blob)

    cues_element = (
        element(_ID_CUES, cues_blob)
        if cues_blob is not None
        else _build_cues(cues or [], offsets, cluster_timecodes)
    )
    body = prefix + b"".join(cluster_blobs) + cues_element

    if write_segment_size:
        segment = element(_ID_SEGMENT, body)
    else:
        # Unknown-size Segment: EBML encodes "unknown" as all value bits set.
        segment = _ID_SEGMENT + b"\x01\xff\xff\xff\xff\xff\xff\xff" + body
    return header + segment


def write_mkv(path: str | Path, *args, **kwargs) -> Path:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(build_mkv(*args, **kwargs))
    return target


def pcm_silence(sample_count: int, channels: int = 2, bit_depth: int = 16) -> bytes:
    """Return zeroed interleaved PCM bytes for ``sample_count`` frames."""
    if bit_depth != 16:
        raise ValueError("only 16-bit PCM fixtures are supported")
    return b"\x00\x00" * (sample_count * channels)


def pcm_tone(sample_count: int, frequency: float, sample_rate: int = 48000,
             channels: int = 2, amplitude: float = 0.25) -> bytes:
    """Return an interleaved 16-bit PCM sine placed on every channel."""
    import math

    frames = bytearray()
    for index in range(sample_count):
        value = int(amplitude * 32767 * math.sin(2 * math.pi * frequency * index / sample_rate))
        sample = struct.pack(">h", value)
        frames += sample * channels
    return bytes(frames)


__all__ = [
    "MkvBlock",
    "MkvCluster",
    "MkvCue",
    "MkvTrack",
    "build_mkv",
    "pcm_silence",
    "pcm_tone",
    "write_mkv",
]
