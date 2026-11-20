"""Segment rendering: policy enforcement, silence holes and deterministic WAV.

This module owns the per-segment output decisions used by the second pipeline
pass.  It is deliberately pure relative to threading: given a decoded segment
plus the programme gain it returns the exact samples to persist and their
status, so the queue runner stays small and the policy rules are unit-testable
on their own.
"""

from __future__ import annotations

import hashlib
from pathlib import Path

import numpy as np
import soundfile as sf

from .audio import AudioConfig, AudioProcessor
from .audio_loudness import expected_samples
from .audio_layout import CANONICAL_LAYOUTS, canonical_layout
from .demux import SegmentDecodeResult, concatenate_chunks
from .errors import Code, DiagnosticCollector, Locator
from .manifest_core import SegmentRef
from .pipeline import Policy
from .timeline import to_float_seconds

WAV_SUBTYPE = "PCM_16"
OUTPUT_PREFIX = "segment"


def locator_for(segment: SegmentRef) -> Locator:
    return Locator(
        manifest_path=str(segment.resolved_path),
        representation=segment.representation,
        sequence=segment.sequence,
        time_range=(
            to_float_seconds(segment.start),
            to_float_seconds(segment.end),
        ),
        segment_ref=segment.raw_ref,
    )


def output_path_for(output_dir: Path, segment: SegmentRef) -> Path:
    name = f"{OUTPUT_PREFIX}-{segment.representation}-{segment.sequence:05d}.wav"
    return Path(output_dir) / name


def write_wav(path: Path, samples: np.ndarray, rate: int) -> str:
    """Write a deterministic PCM-16 WAV and return its SHA-256.

    soundfile with an explicit subtype and contiguous float32 input produces
    stable bytes across runs and machines; zero-sample segments are written as
    valid header-only WAV placeholders so skipped segments remain auditable.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    sf.write(
        path,
        np.ascontiguousarray(samples, dtype=np.float32),
        int(rate),
        subtype=WAV_SUBTYPE,
    )
    return hashlib.sha256(path.read_bytes()).hexdigest()


class SegmentRenderer:
    """Renders planned segments to output arrays under one recovery policy."""

    def __init__(
        self,
        config: AudioConfig,
        output_dir: Path,
        policy: Policy,
        collector: DiagnosticCollector,
    ) -> None:
        self.config = config
        self.output_dir = Path(output_dir)
        self.policy = policy
        self.collector = collector
        target_layout = canonical_layout(config.target_layout)
        assert target_layout is not None
        self.channels = len(CANONICAL_LAYOUTS[target_layout])
        self._used_refs: dict[str, Path] = {}

    def expected_samples(self, segment: SegmentRef) -> int:
        return expected_samples(segment.duration, self.config.target_rate)

    def render(
        self,
        segment: SegmentRef,
        result: SegmentDecodeResult,
        gain_db: float,
    ) -> tuple[str, np.ndarray, int]:
        """Return ``(status, samples, expected_samples)`` for one segment."""
        expected = self.expected_samples(segment)
        if segment.duplicated and segment.raw_ref in self._used_refs:
            reused, _ = sf.read(self._used_refs[segment.raw_ref], always_2d=True)
            return "duplicate", reused.astype(np.float32), expected
        if result.total_samples == 0:
            return self._render_missing(segment, expected)
        samples, rate, layout = concatenate_chunks(result)
        processor = AudioProcessor(
            rate,
            canonical_layout(layout) or "mono",
            self.config,
            source_track=segment.raw_ref,
        )
        normalized = processor.normalize(samples)
        tail = processor.resampler.flush()
        if tail.size:
            normalized = np.concatenate([normalized, tail], axis=0)
        gain = np.float32(10.0 ** (gain_db / 20.0))
        out = (normalized * gain).astype(np.float32)
        if out.shape[0] < expected:
            if self.policy == Policy.SKIP:
                return "recovered", out, expected
            self.collector.add_code(
                Code.TIMELINE_GAP,
                "truncated segment padded with a silent time hole",
                locator=locator_for(segment),
                detail={"padding_samples": expected - out.shape[0]},
            )
            out = np.concatenate(
                [out, np.zeros((expected - out.shape[0], self.channels),
                               dtype=np.float32)],
                axis=0,
            )
        elif out.shape[0] > expected:
            out = out[:expected]
        path = output_path_for(self.output_dir, segment)
        self._used_refs[segment.raw_ref] = path
        return "recovered", out, expected

    def _render_missing(
        self, segment: SegmentRef, expected: int
    ) -> tuple[str, np.ndarray, int]:
        if self.policy == Policy.SKIP:
            return "skipped", np.zeros((0, self.channels), np.float32), expected
        self.collector.add_code(
            Code.TIMELINE_GAP,
            "undecodable segment replaced with a silent time hole",
            locator=locator_for(segment),
            detail={"gap_samples": expected},
        )
        return "gap", np.zeros((expected, self.channels), np.float32), expected


__all__ = ["SegmentRenderer", "WAV_SUBTYPE", "output_path_for", "write_wav"]
