"""Container and video-stream probing without decoding pixels."""

from __future__ import annotations

from dataclasses import asdict, dataclass
from fractions import Fraction
import heapq
from pathlib import Path
from statistics import median
from typing import Any

import av

from frameaudit.errors import InputError


@dataclass(slots=True)
class VideoProbe:
    path: str
    format: str
    codec: str
    width: int
    height: int
    pix_fmt: str
    time_base: str
    nominal_fps: float
    actual_fps: float
    container_average_fps: float | None
    is_vfr: bool
    nominal_interval_us: int
    packet_frames: int
    duration_s: float | None

    def as_dict(self) -> dict[str, Any]:
        return asdict(self)


def _pts_us(pts: int, time_base: Fraction) -> int:
    return round(pts * time_base.numerator * 1_000_000 / time_base.denominator)


def _rate_value(value: Any) -> float | None:
    if value is None:
        return None
    try:
        rate = float(value)
    except (TypeError, ValueError, ZeroDivisionError):
        return None
    return round(rate, 3) if rate > 0 else None


def _median_interval(samples: list[int], fallback: int) -> int:
    positive = [value for value in samples if value > 0]
    if not positive:
        return fallback
    return max(1, int(round(median(positive))))


def probe_video(path: str | Path) -> VideoProbe:
    source = Path(path)
    if not source.is_file():
        raise InputError(f"input does not exist: {source}")

    try:
        container = av.open(str(source))
    except Exception as exc:
        raise InputError(f"cannot open input: {source}: {exc}") from exc

    try:
        if not container.streams.video:
            raise InputError(f"input has no video stream: {source}")
        stream = container.streams.video[0]
        time_base = stream.time_base or Fraction(1, 1000)
        average_fps = _rate_value(stream.average_rate)
        fallback_us = (
            max(1, round(1_000_000 / average_fps))
            if average_fps and 0.1 <= average_fps <= 1000
            else 41_667
        )

        samples: list[int] = []
        packet_frames = 0
        anomalies = 0
        large_intervals = 0
        first_us: int | None = None
        last_us: int | None = None
        previous_us: int | None = None

        pending: list[tuple[int, int]] = []

        def consume(current_us: int) -> None:
            nonlocal packet_frames, anomalies, large_intervals
            nonlocal first_us, last_us, previous_us
            packet_frames += 1
            if first_us is None:
                first_us = current_us
            last_us = current_us
            if previous_us is not None:
                interval = current_us - previous_us
                if interval <= 0:
                    anomalies += 1
                else:
                    if len(samples) < 4096:
                        samples.append(interval)
                    else:
                        slot = (packet_frames * 1_103_515_245 + 12_345) % packet_frames
                        if slot < len(samples):
                            samples[slot] = interval
                    if interval > 1_000_000:
                        large_intervals += 1
            previous_us = current_us

        try:
            for sequence, packet in enumerate(container.demux(stream)):
                if packet.size == 0 or packet.pts is None:
                    continue
                current_us = _pts_us(packet.pts, time_base)
                heapq.heappush(pending, (current_us, sequence))
                if len(pending) > 64:
                    consume(heapq.heappop(pending)[0])
            while pending:
                consume(heapq.heappop(pending)[0])
        except Exception:
            if packet_frames == 0:
                raise InputError(f"cannot read packets: {source}") from None

        nominal_interval_us = _median_interval(samples, fallback_us)
        if samples:
            clock = nominal_interval_us
            threshold = max(clock * 1.5, clock + 2 * round(1_000_000 * float(time_base)))
            vfr = anomalies > 0 or any(
                interval <= 0 or interval > threshold for interval in samples
            )
        else:
            vfr = False

        if packet_frames > 1 and first_us is not None and last_us is not None:
            elapsed = last_us - first_us
            actual_fps = round((packet_frames - 1) * 1_000_000 / elapsed, 3) if elapsed > 0 else 0.0
        else:
            actual_fps = 0.0

        duration_s: float | None = None
        if stream.duration is not None:
            duration_s = round(float(stream.duration * time_base), 6)
        elif container.duration is not None:
            duration_s = round(container.duration / 1_000_000, 6)

        return VideoProbe(
            path=str(source),
            format=(container.format.name or "unknown"),
            codec=stream.codec_context.name or "unknown",
            width=stream.codec_context.width,
            height=stream.codec_context.height,
            pix_fmt=stream.codec_context.pix_fmt or "unknown",
            time_base=f"{time_base.numerator}/{time_base.denominator}",
            nominal_fps=round(1_000_000 / nominal_interval_us, 3),
            actual_fps=actual_fps,
            container_average_fps=average_fps,
            is_vfr=vfr or large_intervals > 0,
            nominal_interval_us=nominal_interval_us,
            packet_frames=packet_frames,
            duration_s=duration_s,
        )
    finally:
        container.close()

