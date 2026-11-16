"""Deterministic network-trace generation.

A single :func:`generate_trace` call produces a trace that walks through
steady-state, a sudden capacity dip, a flapping recovery, a full outage
(zero throughput) and a second recovery with sparse glitch outliers.

Everything derives from a seeded :class:`numpy.random.Generator`, so the
same (segments, segment_seconds, seed) triple always yields byte-identical
samples. No wall clock and no global NumPy random state are used.
"""

from __future__ import annotations

import numpy as np

from .models import ABRConfigError, NetworkSample, Trace

# (start_fraction, end_fraction, base_kbps, rtt_ms, loss, jitter_sigma)
# The base of recovery phases is interpolated linearly between their ends.
_PHASES: tuple[tuple[float, float, float, float, float, float, float], ...] = (
    (0.00, 0.15, 6200.0, 6200.0, 30.0, 0.005, 0.08),   # steady high
    (0.15, 0.30, 4200.0, 4200.0, 45.0, 0.010, 0.10),   # steady mid
    (0.30, 0.42, 850.0, 850.0, 120.0, 0.050, 0.18),    # sudden dip
    (0.42, 0.55, 1200.0, 4000.0, 90.0, 0.030, 0.25),   # flapping recovery
    (0.55, 0.68, 5500.0, 5500.0, 35.0, 0.008, 0.08),   # steady high again
    (0.68, 0.78, 0.0, 0.0, 300.0, 0.600, 0.00),        # total outage
    (0.78, 0.90, 500.0, 4500.0, 150.0, 0.100, 0.20),   # gradual recovery
    (0.90, 1.01, 5200.0, 5200.0, 32.0, 0.006, 0.07),   # stable + glitches
)


def _phase_index(fraction: float) -> int:
    for index, phase in enumerate(_PHASES):
        start, end = phase[0], phase[1]
        if start <= fraction < end:
            return index
    return len(_PHASES) - 1


def generate_trace(
    segments: int,
    segment_seconds: float,
    seed: int,
    name: str = "trace",
) -> Trace:
    """Generate a reproducible trace of ``segments`` network samples."""
    if not isinstance(segments, int) or isinstance(segments, bool):
        raise ABRConfigError(f"segments must be an integer, got {segments!r}")
    if segments < 1:
        raise ABRConfigError(f"segments must be >= 1, got {segments}")
    if not isinstance(seed, int) or isinstance(seed, bool):
        raise ABRConfigError(f"seed must be an integer, got {seed!r}")
    segment_seconds = float(segment_seconds)
    if segment_seconds <= 0 or not np.isfinite(segment_seconds):
        raise ABRConfigError("segment-seconds must be a positive finite number")
    if segment_seconds > 86_400.0:
        raise ABRConfigError("segment-seconds exceeds the 86400s limit")

    rng = np.random.default_rng(seed)
    samples: list[NetworkSample] = []
    for index in range(segments):
        fraction = index / segments
        phase = _PHASES[_phase_index(fraction)]
        start, end, base_lo, base_hi, rtt_base, loss_base, jitter = phase
        progress = 0.0 if end == start else (fraction - start) / (end - start)
        base = base_lo + (base_hi - base_lo) * progress

        throughput = base * (1.0 + jitter * float(rng.standard_normal()))
        rtt = rtt_base * (1.0 + 0.10 * float(rng.standard_normal()))
        loss = loss_base * (1.0 + 0.15 * float(rng.standard_normal()))

        # The dip-exit flaps hard on purpose: verify hysteresis, not luck.
        if _phase_index(fraction) == 3:
            throughput *= 1.0 + 0.35 * float(np.sin(fraction * 24.0 * np.pi))

        # Sparse one-segment glitches in the final stable phase.
        if _phase_index(fraction) == 7 and rng.random() < 0.08:
            throughput *= 2.2 if rng.random() < 0.5 else 0.45

        throughput = max(0.0, round(throughput, 2))
        if base == 0.0:  # outage phase: an exact, sustained dead link
            throughput = 0.0
        rtt = max(0.0, round(rtt, 2))
        loss = round(min(1.0, max(0.0, loss)), 4)

        samples.append(
            NetworkSample(
                t=round(index * segment_seconds, 6),
                throughput=throughput,
                rtt=rtt,
                loss=loss,
            )
        )

    return Trace(
        samples=tuple(samples),
        segment_seconds=segment_seconds,
        seed=seed,
        name=name,
    )
