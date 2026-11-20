"""Deterministic end-to-end pipeline: fixtures, transport, recovery and report.

Every stage is a pure function of :class:`PipelineConfig`.  Random draws are
keyed by the object they affect (track id, frame index, loss slot), never by
iteration order, so rerunning with the same seed reproduces identical
manifests.
"""

from __future__ import annotations

import hashlib
import tempfile
from dataclasses import dataclass, field
from pathlib import Path
from typing import Sequence

from .clock import (
    TICKS_PER_SECOND,
    build_display_track,
    parse_fps,
    ticks_from_rate,
)
from .errors import Gap, InputError
from .fixtures import FixtureConfig, VideoFixture, build_video_fixture, synth_audio
from .ingest import (
    Anchor,
    Limits,
    VideoTrack,
    load_audio_track,
    load_video_track,
    read_stream_slice,
)
from .jitter import JitterConfig, JitterResult, TransportPacket, dejitter
from .recover import (
    RecoveryConfig,
    RecoveryReport,
    RetransmitOracle,
    TrackRecovery,
    recover_interval,
    recover_timeline,
)
from .report import Manifest, TrackManifest, build_manifest
from .sync import (
    DEFAULT_MAX_ERROR_MS,
    SyncConfig,
    SyncReport,
    align_audio_only,
    align_track,
)
from .timeline import Interval, Timeline

MAX_TRACKS = 8
MAX_BYTES = 64 * 1024 * 1024
PCM16_BYTES_PER_SAMPLE = 2
WAV_HEADER_BYTES = 44


@dataclass(frozen=True)
class PipelineConfig:
    seed: int = 17
    codec: str = "h264"
    frames: int = 150
    fps: str = "30"
    width: int = 320
    height: int = 180
    target_bps: int = 4_000_000
    loss: float = 0.08
    jitter_ms: int = 45
    audio_seconds: float | None = None
    sample_rate: int = 48_000
    max_tracks: int = 2
    retransmit_success_rate: float = 0.95
    retransmit_attempts: int = 4
    drift_ppm_limit: float = 500.0
    max_error_ms: float = DEFAULT_MAX_ERROR_MS
    segment_seconds: float = 2.0
    duplicate_rate: float = 0.02

    def __post_init__(self) -> None:
        if not 0.0 <= self.loss < 1.0:
            raise InputError(f"loss must be within [0, 1), got {self.loss}")
        if not 0.0 <= self.duplicate_rate < 1.0:
            raise InputError("duplicate rate must be within [0, 1)")
        if self.jitter_ms < 0:
            raise InputError("jitter budget cannot be negative")
        if self.target_bps <= 0:
            raise InputError("target bitrate must be positive")
        if not 1 <= self.max_tracks <= MAX_TRACKS:
            raise InputError(f"max_tracks must be within 1..{MAX_TRACKS}")
        if self.frames < 2:
            raise InputError("at least two frames are required")
        if parse_fps(self.fps) <= 0:
            raise InputError("frame rate must be positive")
        if self.segment_seconds <= 0:
            raise InputError("segment length must be positive")
        if min(self.sample_rate, self.width, self.height) <= 0:
            raise InputError("dimensions and sample rate must be positive")
        if self.max_error_ms <= 0:
            raise InputError("max alignment error must be positive")
        if self.audio_seconds is not None and self.audio_seconds <= 0:
            raise InputError("audio duration must be positive")
        if not 0.0 <= self.retransmit_success_rate <= 1.0:
            raise InputError("retransmit success rate must be within [0, 1]")
        if self.retransmit_attempts < 0:
            raise InputError("retransmit attempts cannot be negative")

    @property
    def fps_fraction(self):
        return parse_fps(self.fps)

    @property
    def video_seconds(self) -> float:
        return float(self.frames / self.fps_fraction)

    @property
    def duration_seconds(self) -> float:
        return max(self.video_seconds, self.audio_seconds or 0.0)

    @property
    def duration_ticks(self) -> int:
        return int(round(self.duration_seconds * TICKS_PER_SECOND))


@dataclass
class PipelineResult:
    manifest: Manifest
    report: RecoveryReport
    sync: SyncReport
    notes: list[str] = field(default_factory=list)
    workdir: Path | None = None
    frames: int = 0


def _keyed(seed: int, *parts: object) -> int:
    material = "|".join([str(seed), *(str(part) for part in parts)]).encode()
    return int.from_bytes(hashlib.blake2b(material, digest_size=8).digest(), "big")


class DeterministicLoss:
    """Seeded, order-independent loss, jitter and duplicate decisions."""

    def __init__(self, seed: int) -> None:
        self.seed = seed

    def slots(self, count: int, rate: float, track_id: str) -> set[int]:
        if rate <= 0.0 or count <= 0:
            return set()
        target = int(round(count * rate))
        ranked = sorted(
            range(count), key=lambda index: _keyed(self.seed, track_id, "loss", index)
        )
        return set(ranked[:target])

    def offset(self, index: int, budget_ms: int, track_id: str) -> int:
        if budget_ms <= 0:
            return 0
        span = 2 * budget_ms + 1
        return _keyed(self.seed, track_id, "jitter", index) % span - budget_ms

    def duplicate(self, index: int, rate: float, track_id: str) -> bool:
        if rate <= 0.0:
            return False
        threshold = int(rate * 10_000)
        return _keyed(self.seed, track_id, "dup", index) % 10_000 < threshold


def _packet_from_record(
    video: VideoTrack, record, arrival_ms: int
) -> TransportPacket:
    return TransportPacket(
        seq=record.seq % (1 << 16),
        arrival_ms=arrival_ms,
        data=read_stream_slice(video, record),
        keyframe=record.keyframe,
        pts_ticks=record.pts_ticks,
        duration_ticks=record.duration_ticks,
    )


def simulate_transport(
    video: VideoTrack, config: PipelineConfig, loss: DeterministicLoss
) -> JitterResult:
    """Build an arrival-order packet log, then de-jitter it."""

    dropped = loss.slots(len(video.packets), config.loss, video.track_id)
    frame_ms = max(1, int(round(1000.0 / float(video.nominal_rate))))
    arrivals: list[tuple[int, int, TransportPacket]] = []
    for index, record in enumerate(video.packets):
        if index in dropped:
            continue
        arrival = max(
            0, index * frame_ms + loss.offset(index, config.jitter_ms, video.track_id)
        )
        packet = _packet_from_record(video, record, arrival)
        arrivals.append((arrival, index, packet))
        if loss.duplicate(index, config.duplicate_rate, video.track_id):
            arrivals.append((arrival + 1, index, packet))
    arrivals.sort(key=lambda item: (item[0], item[1]))
    return dejitter(
        [item[2] for item in arrivals],
        JitterConfig(wait_budget_ms=config.jitter_ms, reorder_window=512),
    )


def build_intervals(track: VideoTrack, segment_seconds: float) -> list[Interval]:
    """Split a track into dependency-chained intervals on a fixed cadence."""

    step = ticks_from_rate(1, track.nominal_rate)
    segment = max(step, int(round(segment_seconds * TICKS_PER_SECOND)))
    total = track.duration_ticks
    if total <= 0:
        raise InputError(f"{track.track_id}: track has no duration")
    intervals: list[Interval] = []
    previous: str | None = None
    start = 0
    ordinal = 0
    while start < total:
        end = min(total, start + segment)
        interval_id = f"{track.track_id}@seg{ordinal:02d}"
        intervals.append(
            Interval(
                interval_id=interval_id,
                track_id=track.track_id,
                start_ticks=start,
                end_ticks=end,
                kind="video-head" if ordinal == 0 else "video-segment",
                depends_on=(previous,) if previous else (),
            )
        )
        previous = interval_id
        start = end
        ordinal += 1
    return intervals


def allocate_bitrate(
    config: PipelineConfig, tracks: int
) -> tuple[int, int, int]:
    """Split the target into per-video-track and PCM audio budgets.

    Returns ``(per_track_bps, audio_bps, video_bytes_budget)``.  Audio is exact
    PCM, so it is subtracted first; the remainder is divided across video
    tracks.
    """

    samples = int(round(config.duration_seconds * config.sample_rate))
    audio_bytes = WAV_HEADER_BYTES + samples * PCM16_BYTES_PER_SAMPLE
    target_bytes = int(round(config.target_bps * config.duration_seconds / 8.0))
    video_bytes = target_bytes - audio_bytes
    if video_bytes <= 0:
        raise InputError(
            f"target {config.target_bps} bps cannot carry "
            f"{audio_bytes * 8} bits of PCM audio over "
            f"{config.duration_seconds:g}s"
        )
    per_track_bytes = video_bytes // max(1, tracks)
    if per_track_bytes <= 0:
        raise InputError("per-track bitrate budget is empty")
    per_track_bps = int(per_track_bytes * 8 / config.duration_seconds)
    if per_track_bps <= 0:
        raise InputError("per-track bitrate budget rounds to zero")
    return per_track_bps, audio_bytes * 8, video_bytes
