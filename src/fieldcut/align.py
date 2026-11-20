"""Align multi-camera timelines onto one shared media clock.

Each camera contributes a probe result (decode PTS, optional capture-clock
drift) and an AAC audio track. The audio tracks share a deterministic sync
landmark, so normalized cross-correlation fixes the per-camera start offset;
the capture-clock drift then supplies the slope. We never resample, stretch
or drop material: the result is a *mapping* plus confidence intervals, and any
region whose alignment residual exceeds tolerance is explicitly flagged for
human review.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction

import numpy as np

from .errors import ErrorLog, Recovery, Stage
from .probe import ProbeResult, VideoProbe
from .timebase import TICKS_PER_MS, TICKS_PER_SECOND

DEFAULT_TOLERANCE_MS = 120.0
MAX_CORR_LAG_S = 2.0


@dataclass
class CameraAlignment:
    camera: str
    offset_ticks: int                 # shared = local + offset
    drift_rate_ppm: float
    drift_ci_ppm: float
    sync_residual_rms_ms: float
    trusted_start_ticks: int
    trusted_end_ticks: int
    review_ranges: list[tuple[int, int]] = field(default_factory=list)
    sync_lag_samples: int = 0

    def to_shared(self, local_ticks: float) -> float:
        """Map a local decode tick onto the shared timeline."""
        return local_ticks * (1.0 + self.drift_rate_ppm / 1e6) \
            + self.offset_ticks


@dataclass
class AlignmentResult:
    reference_camera: str
    cameras: list[CameraAlignment]
    shared_duration_ticks: int
    tolerance_ms: float

    def for_camera(self, camera: str) -> CameraAlignment:
        return next(c for c in self.cameras if c.camera == camera)


def read_audio_mono(path: str, stream_index: int) -> tuple[np.ndarray, int]:
    """Decode one AAC track to a mono float32 waveform and its sample rate."""
    import av

    container = av.open(path)
    stream = next((s for s in container.streams
                   if s.index == stream_index), None)
    if stream is None:
        container.close()
        raise ValueError(f"audio stream {stream_index} missing in {path}")
    chunks: list[np.ndarray] = []
    rate = stream.codec_context.sample_rate
    try:
        for pkt in container.demux():
            if pkt.stream.index != stream.index:
                continue
            for frame in pkt.decode():
                chunks.append(frame.to_ndarray())
    except av.AVError:
        pass
    finally:
        container.close()
    if not chunks:
        return np.zeros(0, dtype=np.float32), rate
    data = np.concatenate(chunks, axis=1)
    return data.mean(axis=0).astype(np.float32), rate


def cross_correlation_lag(wave: np.ndarray, reference: np.ndarray,
                          sample_rate: int,
                          max_lag_s: float = MAX_CORR_LAG_S) -> int:
    """Integer-sample lag of ``wave`` relative to ``reference`` (FFT based)."""
    if len(wave) < 8 or len(reference) < 8:
        return 0
    n = len(wave) + len(reference) - 1
    nfft = 1 << (n - 1).bit_length()
    a = wave - wave.mean()
    b = reference - reference.mean()
    corr = np.fft.irfft(np.fft.rfft(a, nfft) * np.conj(np.fft.rfft(b, nfft)))
    corr = np.concatenate((corr[-(len(reference) - 1):], corr[: len(wave)]))
    center = len(reference) - 1
    max_lag = min(int(max_lag_s * sample_rate), len(corr) // 2)
    lags = np.arange(-max_lag, max_lag + 1)
    return int(lags[int(np.argmax(corr[center + lags]))])


def _residual_series(video: VideoProbe) -> np.ndarray:
    """Per-frame alignment residual in milliseconds.

    With a fitted capture clock, the residual is each frame's deviation from
    the linear capture→reference drift model (VFR jitter and clock steps show
    up here). Without one the container decode timeline is constant-rate, so
    the residual is uniformly zero; damage is still captured via
    ``damaged_ranges``.
    """
    if video.drift is None or len(video.capture_ticks) < 2:
        return np.zeros(len(video.pts_ticks), dtype=np.float64)
    n = min(len(video.capture_ticks), len(video.pts_ticks))
    ref = np.asarray(video.pts_ticks[:n], dtype=np.float64)
    mapped = np.array([float(video.drift.map(c))
                       for c in video.capture_ticks[:n]])
    out = np.abs(mapped - ref) / TICKS_PER_MS
    if n < len(video.pts_ticks):
        out = np.concatenate([out, np.zeros(len(video.pts_ticks) - n)])
    return out


def align_cameras(
    probes: list[ProbeResult],
    log: ErrorLog,
    tolerance_ms: float = DEFAULT_TOLERANCE_MS,
) -> AlignmentResult:
    """Build the shared timeline. The first camera is the reference."""
    if not probes:
        raise ValueError("align_cameras requires at least one probe")
    ref = probes[0]
    ref_name = ref.videos[0].camera if ref.videos else "ref"

    # Reference mono waveform anchors every cross-correlation.
    ref_mono = np.zeros(0, dtype=np.float32)
    ref_rate = 0
    if ref.audios:
        ref_mono, ref_rate = read_audio_mono(ref.path, stream_index=0)

    alignments: list[CameraAlignment] = []
    shared_end = 0
    for pr in probes:
        video = pr.videos[0]
        lag_samples = 0
        offset_ticks = 0
        if pr.audios and ref_mono.size:
            mono, rate = read_audio_mono(pr.path, 0)
            ref_rate = ref_rate or rate
            lag_samples = cross_correlation_lag(mono, ref_mono, rate)
            # Positive lag: this camera's audio starts later -> local time is
            # behind shared time, so the mapping adds the corresponding ticks.
            offset_ticks = int(round(lag_samples / rate * TICKS_PER_SECOND))
        drift_ppm = float(video.drift.rate_ppm) if video.drift else 0.0
        drift_ci = (float(video.drift.ci_half_width_ppm)
                    if video.drift and np.isfinite(video.drift.ci_half_width_ppm)
                    else 0.0)

        residuals = _residual_series(video)
        rms = float(np.sqrt(np.mean(residuals ** 2))) if residuals.size else 0.0

        start = video.readable_start_ticks
        end = video.readable_end_ticks
        review: list[tuple[int, int]] = []
        tol_ticks = tolerance_ms * TICKS_PER_MS
        # A residual over tolerance flags that frame's surrounding interval.
        over = np.where(residuals > tolerance_ms)[0] if residuals.size else []
        for idx in over:
            t0 = video.pts_ticks[max(0, idx - 1)]
            t1 = video.pts_ticks[min(len(video.pts_ticks) - 1, idx + 1)]
            review.append((t0, t1))
        # Damaged/truncated ranges are never silently trusted.
        for s, e in video.damaged_ranges:
            review.append((s, e))
        review = _merge(review)
        for s, e in review:
            log.add(Stage.ALIGN, video.camera,
                    f"review range {s / TICKS_PER_MS:.1f}-"
                    f"{e / TICKS_PER_MS:.1f} ms exceeds alignment tolerance "
                    f"or covers damaged media",
                    media_time_ticks=s, recovery=Recovery.SEGMENTED)

        trusted_start = int(start + offset_ticks)
        trusted_end = int(end + offset_ticks)
        shared_end = max(shared_end, trusted_end)
        alignments.append(CameraAlignment(
            camera=video.camera, offset_ticks=offset_ticks,
            drift_rate_ppm=drift_ppm, drift_ci_ppm=drift_ci,
            sync_residual_rms_ms=round(rms, 3),
            trusted_start_ticks=trusted_start, trusted_end_ticks=trusted_end,
            review_ranges=review, sync_lag_samples=lag_samples))

    return AlignmentResult(reference_camera=ref_name, cameras=alignments,
                           shared_duration_ticks=shared_end,
                           tolerance_ms=tolerance_ms)


def _merge(ranges: list[tuple[int, int]]) -> list[tuple[int, int]]:
    valid = sorted((s, e) for s, e in ranges if e > s)
    out: list[tuple[int, int]] = []
    for s, e in valid:
        if out and s <= out[-1][1]:
            out[-1] = (out[-1][0], max(out[-1][1], e))
        else:
            out.append((s, e))
    return out


def common_trusted_window(result: AlignmentResult) -> tuple[int, int]:
    """Intersection of every camera's trusted span on the shared clock."""
    if not result.cameras:
        return (0, 0)
    start = max(c.trusted_start_ticks for c in result.cameras)
    end = min(c.trusted_end_ticks for c in result.cameras)
    return (start, max(start, end))
