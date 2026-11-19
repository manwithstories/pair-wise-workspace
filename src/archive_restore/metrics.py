"""Restoration metrics: A/V sync, recovery rate, queue memory and latency.

All time values are integer microseconds internally and reported as whole
milliseconds in the JSON output.  When an audio or video track is missing the
corresponding metric is emitted as ``None`` with an explanatory status rather
than raising, so a one-track archive still produces a coherent report.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path

import numpy as np
import soundfile as sf

from .errors import ErrorCollector, ErrorStage, skippable
from .recovery import RecoveryOutcome

AV_SYNC_LIMIT_US = 40_000
DEFAULT_QUEUE_BUDGET = 512 * 1024 * 1024


@dataclass
class TrackSpan:
    """First and last valid media timestamp (microseconds) for one track."""

    kind: str
    first_us: int | None
    last_us: int | None
    present: bool = True
    note: str = ""


def media_timeline_spans(video_dir: Path, audio_dir: Path,
                         errors: ErrorCollector, asset_id: str = "") -> dict[str, TrackSpan]:
    """Read actual container/audio samples to locate first/last valid times."""
    spans: dict[str, TrackSpan] = {}
    spans["video"] = _video_span(video_dir, errors, asset_id)
    spans["audio"] = _audio_span(audio_dir, errors, asset_id)
    return spans


def _video_span(directory: Path, errors: ErrorCollector, asset_id: str) -> TrackSpan:
    files = sorted(directory.glob("*")) if directory.exists() else []
    files = [f for f in files if f.is_file()]
    if not files:
        return TrackSpan("video", None, None, present=False, note="no video track")
    first = last = None
    import av
    for path in files:
        try:
            container = av.open(str(path))
            stamps = []
            for stream in container.streams.video:
                for frame in container.decode(stream):
                    if frame.pts is None:
                        continue
                    stamps.append(int(frame.pts * 1_000_000 * frame.time_base))
            container.close()
        except (av.AVError, OSError) as exc:
            errors.add(skippable(ErrorStage.METRICS, asset_id, f"unreadable video {path.name}: {exc}",
                                 path=str(path)))
            continue
        if stamps:
            lo, hi = min(stamps), max(stamps)
            first = lo if first is None else min(first, lo)
            last = hi if last is None else max(last, hi)
    if first is None:
        return TrackSpan("video", None, None, present=False, note="no decodable video frames")
    return TrackSpan("video", first, last)


def _audio_span(directory: Path, errors: ErrorCollector, asset_id: str) -> TrackSpan:
    files = sorted(directory.glob("*")) if directory.exists() else []
    files = [f for f in files if f.is_file()]
    if not files:
        return TrackSpan("audio", None, None, present=False, note="no audio track")
    first = last = None
    elapsed = 0
    for path in files:
        try:
            info = sf.info(str(path))
            frames, rate = info.frames, info.samplerate
        except Exception as exc:  # soundfile raises LibsndfileError/RuntimeError
            errors.add(skippable(ErrorStage.METRICS, asset_id, f"unreadable audio {path.name}: {exc}",
                                 path=str(path)))
            continue
        if rate <= 0 or frames <= 0:
            continue
        start_us = int(elapsed * 1_000_000)
        end_us = int((elapsed + frames / rate) * 1_000_000)
        first = start_us if first is None else min(first, start_us)
        last = end_us if last is None else max(last, end_us)
        elapsed += frames / rate
    if first is None:
        return TrackSpan("audio", None, None, present=False, note="no decodable audio samples")
    return TrackSpan("audio", first, last)


def av_sync_error(spans: dict[str, TrackSpan]) -> tuple[int | None, str]:
    """Max of first-frame and last-frame A/V offset in whole milliseconds."""
    video, audio = spans.get("video"), spans.get("audio")
    missing = [k for k in ("video", "audio") if not spans[k].present]
    if missing:
        return None, f"av-sync unavailable: missing {'/'.join(missing)}"
    first_delta = abs(video.first_us - audio.first_us)
    last_delta = abs(video.last_us - audio.last_us)
    return round(max(first_delta, last_delta) / 1000), "ok"


def packet_recovery_rate(outcomes: list[RecoveryOutcome]) -> float:
    expected = sum(o.expected_count for o in outcomes)
    if expected == 0:
        return 1.0
    missing = sum(len(o.gaps) for o in outcomes)
    return round((expected - missing) / expected, 4)


def percentile_latency(latencies_ms: list[float], percentile: float = 95.0) -> float:
    if not latencies_ms:
        return 0.0
    arr = np.asarray(latencies_ms, dtype=np.float64)
    return round(float(np.percentile(arr, percentile)), 3)


@dataclass
class MetricsReport:
    av_sync_error_ms: int | None
    av_sync_status: str
    packet_recovery_rate: float
    p95_latency_ms: float
    peak_memory_bytes: int
    aligned_representations: int
    max_alignment_deviation_us: int
    gaps: list[dict] = field(default_factory=list)
    missing_tracks: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        return {
            "av_sync_error_ms": self.av_sync_error_ms,
            "av_sync_status": self.av_sync_status,
            "packet_recovery_rate": self.packet_recovery_rate,
            "p95_latency_ms": self.p95_latency_ms,
            "peak_memory_bytes": self.peak_memory_bytes,
            "aligned_representations": self.aligned_representations,
            "max_alignment_deviation_us": self.max_alignment_deviation_us,
            "gaps": self.gaps,
            "missing_tracks": self.missing_tracks,
        }

    def meets_thresholds(self, recovery_min: float = 0.95) -> tuple[bool, list[str]]:
        failures: list[str] = []
        if self.av_sync_error_ms is not None and self.av_sync_error_ms > AV_SYNC_LIMIT_US // 1000:
            failures.append(f"av_sync_error_ms {self.av_sync_error_ms} > 40")
        if self.packet_recovery_rate < recovery_min:
            failures.append(f"packet_recovery_rate {self.packet_recovery_rate} < {recovery_min}")
        return not failures, failures
