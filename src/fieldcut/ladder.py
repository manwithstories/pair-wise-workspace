"""Multi-bitrate encoding ladder with verifiable per-segment metrics.

For every camera and every target bitrate the source is decoded once and each
planned segment is written as its own MP4 with an IDR at the start, so the
manifest can address segments independently. Encoding goes to a temporary file
and is atomically renamed only on success; a failed or cancelled segment never
leaves a partial output under its final name.

Quality is measured as PSNR against the deterministic reference picture
regenerated from the frame index, so no pristine copy of the media has to be
stored. When a segment contains no decodable reference frame (fully damaged),
PSNR is reported as ``null`` with an explicit reason instead of a fabricated
number.
"""

from __future__ import annotations

import os
import tempfile
from dataclasses import dataclass, field
from fractions import Fraction

import av
import numpy as np

from .align import AlignmentResult
from .errors import ErrorLog, Recovery, Stage
from .probe import ProbeResult
from .segment import SegmentationPlan
from .timebase import TICKS_PER_MS, TICKS_PER_SECOND

FPS = 24


@dataclass
class SegmentMetrics:
    index: int
    representation: int
    target_bps: int
    actual_bps: int
    bitrate_error_pct: float
    peak_bps: int
    duration_ms: float
    psnr_db: float | None
    psnr_reason: str | None
    frames: int
    path: str


@dataclass
class RepresentationResult:
    camera: str
    representation: int
    target_bps: int
    width: int
    height: int
    segments: list[SegmentMetrics] = field(default_factory=list)


def _psnr(reference: np.ndarray, coded: np.ndarray) -> float:
    """Mean PSNR across RGB planes; ``inf`` for an exact match."""
    diff = reference.astype(np.float64) - coded.astype(np.float64)
    mse = float(np.mean(diff ** 2))
    if mse <= 1e-12:
        return float("inf")
    return float(10.0 * np.log10((255.0 ** 2) / mse))


def _decode_frames(probe: ProbeResult) -> list[tuple[int, np.ndarray]]:
    """Decode every readable video frame as (pts_ticks, rgb ndarray)."""
    path = probe.path
    stream = next(s for s in probe_videos(probe))
    tb = Fraction(stream.time_base)
    container = av.open(path)
    out: list[tuple[int, np.ndarray]] = []
    try:
        for pkt in container.demux():
            if pkt.stream.index != stream.index or pkt.pts is None:
                continue
            try:
                frames = pkt.decode()
            except av.AVError:
                continue
            for frame in frames:
                rgb = frame.to_ndarray(format="rgb24")
                pts_ticks = int(round(float(pkt.pts * tb) * TICKS_PER_SECOND))
                out.append((pts_ticks, rgb))
    except av.AVError:
        pass
    finally:
        container.close()
    return out


def probe_videos(probe: ProbeResult):
    container = av.open(probe.path)
    streams = [s for s in container.streams if s.type == "video"]
    container.close()
    return streams


def _frame_index_for_pts(pts_ticks: int) -> int:
    """Recover the source frame index from a constant-rate decode timeline."""
    return int(round(pts_ticks / TICKS_PER_SECOND * FPS))


def _atomic_output(final_path: str, write_fn) -> None:
    """Write via a sibling temp file, then atomically rename into place."""
    directory = os.path.dirname(final_path) or "."
    os.makedirs(directory, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".seg-", suffix=".tmp.mp4",
                               dir=directory)
    os.close(fd)
    try:
        write_fn(tmp)
        os.replace(tmp, final_path)
    except BaseException:
        if os.path.exists(tmp):
            os.unlink(tmp)
        raise
