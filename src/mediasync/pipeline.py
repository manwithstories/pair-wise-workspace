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


def _anchor_seconds(frames: int, fps: str) -> list[float]:
    rate = parse_fps(fps)
    step = max(1, frames // 4)
    points = sorted({step, 2 * step, 3 * step} & set(range(1, frames)))
    return [float(index / rate) for index in points]


def _build_calibrated(
    root: Path,
    fixture_config: FixtureConfig,
    *,
    track_id: str,
    role: str,
    anchors_seconds: Sequence[float],
    target_bytes: int,
    capture_offset_ticks: int = 0,
    drift_ppm: float = 0.0,
    max_passes: int = 2,
) -> VideoFixture:
    """Encode a track, then nudge the encoder bitrate toward ``target_bytes``.

    Encoder CBR accuracy differs by codec (H.264 filler is near exact, HEVC and
    AV1 undershoot or overshoot several percent), so a closed loop is used
    instead of trusting the requested bitrate.
    """

    bitrate = max(1, int(round(target_bytes * 8 / fixture_config.duration_seconds)))
    fixture: VideoFixture | None = None
    for attempt in range(max_passes + 1):
        fixture = build_video_fixture(
            root,
            fixture_config,
            track_id=track_id,
            role=role,
            anchors_seconds=anchors_seconds,
            bitrate=bitrate,
            capture_offset_ticks=capture_offset_ticks,
            drift_ppm=drift_ppm,
        )
        if target_bytes <= 0:
            return fixture
        deviation = (fixture.size_bytes - target_bytes) / target_bytes
        if abs(deviation) <= 0.01 or attempt == max_passes:
            return fixture
        bitrate = max(1, int(round(bitrate * target_bytes / fixture.size_bytes)))
    assert fixture is not None
    return fixture


def run_pipeline(
    config: PipelineConfig,
    *,
    workdir: str | Path | None = None,
    keep_workdir: bool = False,
) -> PipelineResult:
    """Generate fixtures, simulate transport, recover, synchronize and report."""

    fps = config.fps_fraction
    duration_seconds = config.duration_seconds
    track_count = min(config.max_tracks, MAX_TRACKS)
    per_track_bps, _audio_bps, video_budget = allocate_bitrate(config, track_count)
    per_track_bytes = video_budget // track_count
    limits = Limits(
        max_stream_bytes=MAX_BYTES,
        max_audio_seconds=max(120.0, duration_seconds + 1.0),
    )
    temp = None
    if workdir is None:
        temp = tempfile.TemporaryDirectory(prefix="mediasync-")
        root = Path(temp.name)
    else:
        root = Path(workdir)
        root.mkdir(parents=True, exist_ok=True)
    try:
        anchor_seconds = _anchor_seconds(config.frames, config.fps)
        results: list[tuple[VideoTrack, VideoFixture, JitterResult]] = []
        for index in range(track_count):
            role = ("camera", "interview", "remote")[index % 3]
            fixture_config = FixtureConfig(
                seed=config.seed,
                width=config.width,
                height=config.height,
                frames=config.frames,
                fps=fps,
                codec=config.codec,
                target_bps=per_track_bps,
                audio_seconds=duration_seconds,
                sample_rate=config.sample_rate,
            )
            # Camera 0 is the program reference; remote feeds carry a small
            # capture offset and a sub-limit clock drift, so synchronization
            # is exercised with non-zero (but passing) error.
            offset_ms = (0.0, 4.0, 7.5)[index % 3]
            drift_ppm = (0.0, 35.0, -60.0)[index % 3]
            fixture = _build_calibrated(
                root,
                fixture_config,
                track_id=f"video{index}",
                role=role,
                anchors_seconds=anchor_seconds,
                target_bytes=per_track_bytes,
                capture_offset_ticks=int(round(offset_ms * TICKS_PER_SECOND / 1000)),
                drift_ppm=drift_ppm,
            )
            video = load_video_track(fixture.index_path, limits=limits)
            jitter = simulate_transport(
                video, config, DeterministicLoss(config.seed)
            )
            results.append((video, fixture, jitter))

        audio_path = root / "program.wav"
        synth_audio(
            audio_path,
            seed=config.seed,
            seconds=duration_seconds,
            sample_rate=config.sample_rate,
        )
        audio = load_audio_track(
            audio_path, track_id="program", role="program", limits=limits
        )
        audio.anchors = tuple(
            Anchor(
                label=f"a{index}",
                audio_sample=int(round(seconds * config.sample_rate)),
                audio_seconds=seconds,
            )
            for index, seconds in enumerate(anchor_seconds)
        )
        audio_size = audio_path.stat().st_size

        timeline = Timeline()
        recovery_config = RecoveryConfig(
            seed=config.seed,
            max_retransmit_attempts=config.retransmit_attempts,
            retransmit_success_rate=config.retransmit_success_rate,
        )
        oracle = RetransmitOracle(recovery_config)
        recoveries: dict[str, TrackRecovery] = {}
        display_tracks: dict[str, list[int]] = {}
        track_intervals: dict[str, list[Interval]] = {}
        resolvers: dict[str, dict[int, TransportPacket]] = {}
        for video, _fixture, jitter in results:
            step = ticks_from_rate(1, video.nominal_rate)
            display = build_display_track(
                [packet.pts_ticks for packet in video.packets],
                nominal_step_ticks=step,
            )
            display_tracks[video.track_id] = display.display_ticks
            intervals = build_intervals(video, config.segment_seconds)
            track_intervals[video.track_id] = intervals
            # Lost packets remain available at the sender, so a successful
            # retransmission restores the exact bytes.
            by_seq = {packet.seq: packet for packet in video.packets}
            resolver = {
                item.seq: _packet_from_record(video, by_seq[item.seq], item.deadline_ms)
                for item in jitter.missing
                if item.seq in by_seq
            }
            resolvers[video.track_id] = resolver
            for interval in intervals:
                timeline.add(interval)
                recoveries[interval.interval_id] = recover_interval(
                    interval,
                    ordered=jitter.ordered,
                    missing=jitter.missing,
                    truncated_seqs=video.truncated_indices,
                    config=recovery_config,
                    oracle=oracle,
                    resolver=resolver.get,
                )
        timeline.validate()
        recovery = recover_timeline(timeline, recoveries)

        sync_config = SyncConfig(
            max_error_ms=config.max_error_ms,
            drift_limit_ppm=config.drift_ppm_limit,
        )
        sync_report = SyncReport()
        for video, _fixture, _jitter in results:
            segments = [
                segment
                for segment in recovery.segments
                if segment.track_id == video.track_id
            ]
            head = segments[0] if segments else None
            sync_report.tracks.append(
                align_track(video, audio, segment=head, config=sync_config)
            )
        sync_report.tracks.append(align_audio_only(audio, 0, config=sync_config))

        unrecovered_seqs: dict[str, set[int]] = {video.track_id: set() for video, _f, _j in results}
        for segment in recovery.segments:
            unrecovered_seqs[segment.track_id].update(segment.loss_seqs)
        delivered_bytes = audio_size
        for video, fixture, jitter in results:
            sizes = {packet.seq: packet.size for packet in video.packets}
            lost_bytes = sum(
                sizes.get(seq, 0) for seq in unrecovered_seqs[video.track_id]
            )
            delivered_bytes += max(0, fixture.size_bytes - lost_bytes)

        loss_total = sum(len(jitter.missing) for _v, _f, jitter in results)
        recovered_total = recovery.recovered
        unrecoverable_total = sum(
            len(segment.loss_seqs) for segment in recovery.segments
        )
        peak, rms = audio.peak_rms(limits)
        entries = _track_entries(results, recovery, sync_report, audio)
        duration_ticks = max(
            config.duration_ticks,
            max(
                [max(item.end_ticks for item in intervals) for intervals in track_intervals.values()]
                + [audio.duration_ticks]
            ),
        )
        manifest = build_manifest(
            seed=config.seed,
            target_bps=config.target_bps,
            total_bytes=delivered_bytes,
            duration_ticks=duration_ticks,
            tracks=entries,
            gaps=[gap.to_dict() for gap in _dedupe(recovery.gaps)],
            transport={
                "loss": loss_total,
                "recovered": recovered_total,
                "unrecoverable": unrecoverable_total,
                "retransmit_attempts": recovery.attempts,
                "duplicates": sum(jitter.duplicates for _v, _f, jitter in results),
                "reordered": sum(jitter.reordered for _v, _f, jitter in results),
                "late": sum(len(jitter.late) for _v, _f, jitter in results),
                "conflicts": sum(len(jitter.conflicts) for _v, _f, jitter in results),
            },
            audio={
                "sample_rate": audio.sample_rate,
                "channels": audio.channels,
                "frames": audio.frames,
                "seconds": round(audio.frames / audio.sample_rate, 6),
                "peak": round(peak, 6),
                "rms": round(rms, 6),
            },
            sync={
                "max_error_ms": round(sync_report.max_error_ms, 6),
                "aligned": sum(1 for item in sync_report.tracks if item.aligned),
                "tracks": [item.to_dict() for item in sync_report.tracks],
            },
            notes=[
                f"codec={config.codec}",
                f"fps={config.fps} frames={config.frames}",
                f"loss={config.loss:.3f} jitter_ms={config.jitter_ms}",
                f"video_tracks={len(results)} per_track_bps={per_track_bps}",
                f"segment_seconds={config.segment_seconds:g}",
            ],
        )
        return PipelineResult(
            manifest=manifest,
            report=recovery,
            sync=sync_report,
            notes=manifest.notes,
            workdir=root,
            frames=len(results[0][0].packets) if results else 0,
        )
    finally:
        if temp is not None and not keep_workdir:
            temp.cleanup()
