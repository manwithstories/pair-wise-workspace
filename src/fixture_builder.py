"""Programmatic Matroska fixture generation.

Sample media is generated in-memory and written to a caller-chosen path (or
returned as bytes). No external files are referenced.

The default (``demo_spec``) profile is deliberately hostile: unknown-size
Segment and Clusters, negative first timestamps, a duplicated presentation
time, interlaced video/audio blocks, and Cues whose cluster positions are
computed from the encoded byte layout — so a remuxer that assumes "Cues point
at zero" produces a visibly wrong mapping.
"""

from __future__ import annotations

import hashlib
from dataclasses import dataclass, field
from pathlib import Path

from . import ebml_schema as ids
from ._fixture_ebml import (
    binary_elem,
    block_group,
    elem,
    elem_unknown,
    float_elem,
    simple_block,
    string_elem,
    uint_elem,
)
from ._fixture_samples import default_codec_private, sample_bytes

# ── element writers ───────────────────────────────────────────────────────


@dataclass
class FixtureTrack:
    number: int
    uid: int
    kind: str  # "video" | "audio" | "subtitle"
    codec_id: str
    language: str = "und"
    default: bool = True
    default_duration_ns: int | None = None
    width: int = 320
    height: int = 180
    sample_rate: float = 48_000.0
    channels: int = 2
    codec_private: bytes = b""
    include_uid: bool = True
    #: Timestamps in TimecodeScale ticks, one per frame.
    ticks: list[int] = field(default_factory=list)
    payload_size: int = 24


@dataclass
class FixtureSpec:
    """Shape of the fixture to emit."""

    scale_ns: int = 1_000_000
    duration_ticks: float | None = None
    unknown_segment: bool = True
    unknown_clusters: bool = True
    include_cues: bool = False
    include_attachments: bool = False
    include_seekhead: bool = False
    tracks: list[FixtureTrack] = field(default_factory=list)
    #: Cluster grouping: how many samples per cluster, round-robin across tracks.
    samples_per_cluster: int = 4
    empty_cluster_at_start: bool = False
    title: str = "mkv2mp4 fixture"


def _track_entry(track: FixtureTrack, *, omit_uid: bool = False) -> bytes:
    payload = uint_elem(ids.TRACK_NUMBER, track.number)
    if track.include_uid and not omit_uid:
        payload += uint_elem(ids.TRACK_UID, track.uid)
    payload += uint_elem(ids.TRACK_TYPE, {"video": 1, "audio": 2, "subtitle": 17}[track.kind])
    payload += uint_elem(ids.FLAG_DEFAULT, 1 if track.default else 0)
    payload += uint_elem(ids.FLAG_LACING, 0)
    payload += string_elem(ids.CODEC_ID, track.codec_id)
    if track.default_duration_ns:
        payload += uint_elem(ids.DEFAULT_DURATION, track.default_duration_ns)
    if track.language != "und":
        payload += string_elem(ids.LANGUAGE, track.language)
    private = track.codec_private or default_codec_private(track.codec_id)
    if private:
        payload += binary_elem(ids.CODEC_PRIVATE, private)
    if track.kind == "video":
        payload += elem(
            ids.VIDEO,
            uint_elem(ids.PIXEL_WIDTH, track.width) + uint_elem(ids.PIXEL_HEIGHT, track.height),
        )
    elif track.kind == "audio":
        payload += elem(
            ids.AUDIO,
            float_elem(ids.SAMPLING_FREQUENCY, track.sample_rate)
            + uint_elem(ids.CHANNELS, track.channels),
        )
    return elem(ids.TRACK_ENTRY, payload)


def _sample_bytes(track: "FixtureTrack", index: int) -> bytes:
    return sample_bytes(track.number, index, track.codec_id, track.payload_size)


def _cluster_payload(
    spec: FixtureSpec,
    cluster_index: int,
    timecode: int,
    entries: list[tuple[FixtureTrack, int]],
) -> bytes:
    """Serialize one Cluster body: its timestamp plus the scheduled blocks."""
    body = uint_elem(ids.TIMESTAMP, max(0, timecode))
    for track, sample_index in entries:
        relative = track.ticks[sample_index] - timecode
        data = _sample_bytes(track, sample_index)
        if track.kind == "video" and sample_index % 5 == 1:
            # A non-keyframe video sample expressed as a BlockGroup.
            body += block_group(track.number, relative, data, references=(0,))
        else:
            body += simple_block(
                track.number,
                relative,
                data,
                keyframe=track.kind != "video" or sample_index % 5 == 0,
            )
    return body


def build_fixture(spec: FixtureSpec) -> bytes:
    """Serialize ``spec`` into a Matroska byte string."""
    header = elem(
        ids.EBML_HEADER,
        uint_elem(ids.EBML_VERSION, 1)
        + uint_elem(ids.EBML_READ_VERSION, 1)
        + uint_elem(ids.EBML_MAX_ID_LENGTH, 4)
        + uint_elem(ids.EBML_MAX_SIZE_LENGTH, 8)
        + string_elem(ids.DOC_TYPE, "matroska")
        + uint_elem(ids.DOC_TYPE_VERSION, 4)
        + uint_elem(ids.DOC_TYPE_READ_VERSION, 2),
    )
    info_payload = uint_elem(ids.TIMESTAMP_SCALE, spec.scale_ns)
    info_payload += string_elem(ids.MUXING_APP, "mkv2mp4-fixture", utf8=True)
    info_payload += string_elem(ids.WRITING_APP, "mkv2mp4-fixture", utf8=True)
    if spec.duration_ticks is not None:
        info_payload += float_elem(ids.DURATION, spec.duration_ticks)
    if spec.title:
        info_payload += string_elem(ids.TITLE, spec.title, utf8=True)
    info_payload += binary_elem(ids.SEGMENT_UID, hashlib.sha1(b"mkv2mp4").digest()[:16])

    tracks_payload = b"".join(_track_entry(t) for t in spec.tracks)

    clusters: list[bytes] = []
    cluster_times: list[int] = []
    if spec.empty_cluster_at_start:
        clusters.append(elem(ids.CLUSTER, uint_elem(ids.TIMESTAMP, 0)))
        cluster_times.append(0)

    # Interleave tracks round-robin by sample index. Per-track order is the
    # order given in ``ticks`` — deliberately NOT sorted, so duplicate and
    # backwards timestamps survive into the fixture the way a bad muxer emits
    # them (a within-cluster negative relative timecode is legal Matroska).
    schedule: list[tuple[FixtureTrack, int]] = []
    longest = max((len(t.ticks) for t in spec.tracks), default=0)
    for index in range(longest):
        for track in spec.tracks:
            if index < len(track.ticks):
                schedule.append((track, index))

    per_cluster = max(1, spec.samples_per_cluster)
    for start in range(0, len(schedule), per_cluster):
        chunk = schedule[start : start + per_cluster]
        # A Cluster Timestamp is unsigned, so negative source times are
        # expressed as a negative Block relative timecode inside a cluster
        # whose own timestamp is clamped to zero.
        timecode = max(0, min(track.ticks[index] for track, index in chunk))
        body = _cluster_payload(spec, len(clusters), timecode, chunk)
        clusters.append(elem_unknown(ids.CLUSTER, body) if spec.unknown_clusters else elem(ids.CLUSTER, body))
        cluster_times.append(timecode)

    seekhead = b""
    if spec.include_seekhead:
        seekhead = elem(
            ids.SEEK_HEAD,
            elem(ids.SEEK, binary_elem(ids.SEEK_ID, ids.INFO) + uint_elem(ids.SEEK_POSITION, 0)),
        )

    # Cue positions are relative to the first byte of Segment data, so they are
    # computed from the actual encoded prefix rather than assumed.
    segment_prefix = (
        len(seekhead) + len(elem(ids.INFO, info_payload)) + len(elem(ids.TRACKS, tracks_payload))
    )
    cues = b""
    if spec.include_cues:
        cue_points: list[bytes] = []
        video_track = next((t for t in spec.tracks if t.kind == "video"), spec.tracks[0])
        cursor = segment_prefix
        for encoded, timecode in zip(clusters, cluster_times):
            cue_points.append(
                elem(
                    ids.CUE_POINT,
                    uint_elem(ids.CUE_TIME, timecode)
                    + elem(
                        ids.CUE_TRACK_POSITIONS,
                        uint_elem(ids.CUE_TRACK, video_track.number)
                        + uint_elem(ids.CUE_CLUSTER_POSITION, cursor),
                    ),
                )
            )
            cursor += len(encoded)
        cues = elem(ids.CUES, b"".join(cue_points))

    attachments = b""
    if spec.include_attachments:
        attachments = elem(
            ids.ATTACHMENTS,
            elem(
                ids.ATTACHED_FILE,
                string_elem(ids.FILE_NAME, "cover.png", utf8=True)
                + string_elem(ids.FILE_MIME_TYPE, "image/png")
                + uint_elem(ids.FILE_UID, 1)
                + binary_elem(ids.FILE_DATA, b"\x89PNG\r\n\x1a\n"),
            ),
        )

    segment_body = (
        seekhead
        + elem(ids.INFO, info_payload)
        + elem(ids.TRACKS, tracks_payload)
        + b"".join(clusters)
        + cues
        + attachments
    )
    if spec.unknown_segment:
        segment = elem_unknown(ids.SEGMENT, segment_body)
    else:
        segment = elem(ids.SEGMENT, segment_body)
    return header + segment


def _all_ticks(spec: FixtureSpec) -> list[int]:
    return [tick for track in spec.tracks for tick in track.ticks]


def demo_spec() -> FixtureSpec:
    from ._fixture_profiles import demo_spec as _demo

    return _demo()


def subtitle_spec() -> FixtureSpec:
    from ._fixture_profiles import subtitle_spec as _sub

    return _sub()


def minimal_spec() -> FixtureSpec:
    from ._fixture_profiles import minimal_spec as _min

    return _min()


def write_fixture(path: str | Path, spec: FixtureSpec | None = None) -> Path:
    """Write ``spec`` (default: :func:`demo_spec`) to ``path`` and return it."""
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    out.write_bytes(build_fixture(spec or demo_spec()))
    return out


def fixture_sha256(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


__all__ = [
    "FixtureSpec",
    "FixtureTrack",
    "block_group",
    "build_fixture",
    "demo_spec",
    "elem",
    "elem_unknown",
    "fixture_sha256",
    "minimal_spec",
    "simple_block",
    "subtitle_spec",
    "write_fixture",
]
