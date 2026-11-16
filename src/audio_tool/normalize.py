"""Gain calculation, true-peak protection and clipping accounting.

Two passes over a resampled intermediate WAV:

  pass 1  input -> resampler -> intermediate, measuring pre-gain loudness and
          the clipping present in the *original* samples
  pass 2  intermediate -> gain -> final WAV, measuring post-gain loudness and
          the clipping actually present in the output

A single pass cannot work: the gain depends on the whole-file gated loudness,
which is only known at the end, and holding the resampled signal in memory
would break the block-processing budget.  Re-running the resampler twice would
double the most expensive stage, hence the intermediate.

Clipping is reported in three independent categories:

  ``original_clipped``  samples at/above full scale in the input file
  ``output_clipped``    samples at/above full scale in the written output
  ``added_clipped``     output clipping that was not clipping before the gain

True-peak protection works by *reducing the gain*, never by clamping samples:
clamping would add harmonic distortion, while a lower gain is transparent.
A correctly protected file therefore reports ``output_clipped == 0``; a
non-zero value means the ceiling could not be honoured (for example a
float input already above full scale at unity gain).
"""

from __future__ import annotations

import array
import math
import os
import tempfile
from array import array as _double_array
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable, Iterator, Sequence

from . import io as audio_io
from .loudness import LoudnessMeter
from .model import (
    AudioBlock,
    ChannelLayout,
    LoudnessStats,
    ProcessConfig,
    WriteError,
)
from .resampler import PolyphaseResampler

#: Safety margin below the requested ceiling, in dB, absorbing the difference
#: between the interpolator's reading and the analog peak.
TRUE_PEAK_MARGIN_DB = 0.1


@dataclass
class NormalizeResult:
    """Everything measured about one file's normalization."""

    gain_db: float
    requested_gain_db: float
    true_peak_limited: bool
    before: LoudnessStats
    after: LoudnessStats
    original_clipped: int
    output_clipped: int
    added_clipped: int
    zero_padded_frames: int
    input_frames: int
    output_frames: int
    input_rate: int
    input_channels: int
    resampler: dict = field(default_factory=dict)

    @property
    def gain_linear(self) -> float:
        return 10.0 ** (self.gain_db / 20.0)

    @property
    def status(self) -> str:
        """``ok``, ``silent`` (passed through at unity) or ``limited``."""

        if self.before.digital_silence or self.before.lufs is None:
            return "silent"
        if self.true_peak_limited:
            return "limited"
        return "ok"

    @property
    def note(self) -> str | None:
        if self.status == "silent":
            return ("digital silence or below the -70 LUFS gate: loudness is "
                    "undefined, so the file passes through at unity gain")
        if self.status == "limited":
            return ("gain reduced to honour the true-peak ceiling; measured "
                    f"loudness lands above the {self.requested_gain_db:+.2f} dB "
                    "request implies")
        return None

    def to_dict(self) -> dict:
        return {
            "status": self.status,
            "note": self.note,
            "gain_db": round(self.gain_db, 4),
            "requested_gain_db": round(self.requested_gain_db, 4),
            "true_peak_limited": self.true_peak_limited,
            "input_rate": self.input_rate,
            "input_channels": self.input_channels,
            "before": self.before.to_dict(),
            "after": self.after.to_dict(),
            "original_clipped": self.original_clipped,
            "output_clipped": self.output_clipped,
            "added_clipped": self.added_clipped,
            "zero_padded_frames": self.zero_padded_frames,
            "input_frames": self.input_frames,
            "output_frames": self.output_frames,
            "resampler": self.resampler,
        }


def compute_gain(before: LoudnessStats, config: ProcessConfig) -> tuple[float, float, bool]:
    """Return ``(applied_gain_db, requested_gain_db, true_peak_limited)``.

    Material below the absolute gate (including digital silence) has no
    defined loudness, so no gain can move it toward the target.  It is passed
    through at unity rather than rejected: a silent stem in a delivery set is
    normal, and the report must still carry its (defined, finite) statistics.
    ``before.digital_silence`` is the flag callers check to explain why the
    gain is 0 dB.
    """

    if before.lufs is None or before.digital_silence:
        return 0.0, 0.0, False
    requested = config.target_lufs - before.lufs
    ceiling = _ceiling_linear(config)
    if before.true_peak <= 0.0:
        return requested, requested, False
    limit_db = 20.0 * math.log10(ceiling / before.true_peak)
    if limit_db < requested:
        return limit_db, requested, True
    return requested, requested, False


def _ceiling_linear(config: ProcessConfig) -> float:
    return 10.0 ** ((config.true_peak_db - TRUE_PEAK_MARGIN_DB) / 20.0)


def scale_block(planes: Sequence[Sequence[float]], frames: int,
                gain: float) -> list[_double_array]:
    """Multiply every sample by ``gain`` (plane-major, frame-bounded)."""

    return [_double_array("d", [value * gain for value in plane[:frames]])
            for plane in planes]


def run_two_pass(
    source: Path,
    destination: Path,
    config: ProcessConfig,
    *,
    kernel: PolyphaseResampler | None = None,
    log: Callable[[str], None] | None = None,
) -> NormalizeResult:
    """Normalize one file end to end.

    ``kernel`` lets a batch share one configured resampler instance, satisfying
    the requirement that every file reuse the same kernel configuration; the
    streaming state inside it is still per file.
    """

    config.validate()
    source = Path(source)
    info = audio_io.probe(source)
    audio_io.guard_output_path(info.path, destination)
    if kernel is None:
        kernel = PolyphaseResampler(config.resampler, info.samplerate,
                                    config.target_rate)

    subtype = config.output_subtype or audio_io.DEFAULT_WRITE_SUBTYPE
    intermediate = _intermediate_path(destination)
    try:
        before, original_clipped, padded = _first_pass(
            info, config, kernel, intermediate, log)
        gain_db, requested_db, limited = compute_gain(before, config)
        after = _second_pass(intermediate, destination, config, gain_db,
                             info.layout, subtype, log)
    finally:
        _unlink(intermediate)

    added = max(0, after.clipped_samples - before.clipped_samples)
    return NormalizeResult(
        gain_db=gain_db,
        requested_gain_db=requested_db,
        true_peak_limited=limited,
        before=before,
        after=after,
        original_clipped=original_clipped,
        output_clipped=after.clipped_samples,
        added_clipped=added,
        zero_padded_frames=padded,
        input_frames=info.frames,
        output_frames=after.frames,
        input_rate=info.samplerate,
        input_channels=info.channels,
        resampler=kernel.signature(),
    )


def _first_pass(info: audio_io.AudioInfo, config: ProcessConfig,
                kernel: PolyphaseResampler, intermediate: Path,
                log: Callable[[str], None] | None):
    """Resample into ``intermediate`` while measuring pre-gain statistics."""

    meter = LoudnessMeter(info.channels, config.target_rate, info.layout.weights)
    counters = {"clipped": 0, "padded": 0}
    stream = kernel.stream()

    def emit() -> Iterator[AudioBlock]:
        with audio_io.BlockReader(info, config.block_frames) as reader:
            for block in reader:
                # Clipping is counted on the *original* samples: after
                # resampling, ringing can both add and hide full-scale peaks,
                # so the input is the only honest place to count it.
                for plane in block.planes:
                    counters["clipped"] += audio_io.count_clipped(
                        plane[:block.frames])
                for resampled in stream.push(block):
                    meter.process(resampled.planes, resampled.frames)
                    yield resampled
            counters["padded"] = reader.zero_padded_frames
        for resampled in stream.flush():
            meter.process(resampled.planes, resampled.frames)
            yield resampled

    audio_io.write_wav_atomic(intermediate, emit(), config.target_rate,
                              info.channels, "PCM_32")
    before = meter.finish()
    if log:
        lufs = "-inf" if before.lufs is None else f"{before.lufs:.2f}"
        log(f"  pass 1: {info.frames} frames -> {before.frames} @ "
            f"{config.target_rate} Hz, {lufs} LUFS")
    return before, counters["clipped"], counters["padded"]


def _second_pass(intermediate: Path, destination: Path, config: ProcessConfig,
                 gain_db: float, layout: ChannelLayout, subtype: str,
                 log: Callable[[str], None] | None):
    """Apply gain, write the final file, and measure it."""

    info = audio_io.probe(intermediate)
    gain = 10.0 ** (gain_db / 20.0)
    meter = LoudnessMeter(info.channels, config.target_rate, layout.weights)

    def emit() -> Iterator[AudioBlock]:
        with audio_io.BlockReader(info, config.block_frames) as reader:
            for block in reader:
                scaled = scale_block(block.planes, block.frames, gain)
                meter.process(scaled, block.frames)
                yield AudioBlock(planes=scaled, frames=block.frames)

    audio_io.write_wav_atomic(destination, emit(), config.target_rate,
                              info.channels, subtype)
    after = meter.finish()
    if log:
        lufs = "-inf" if after.lufs is None else f"{after.lufs:.2f}"
        log(f"  pass 2: gain {gain_db:+.2f} dB -> {lufs} LUFS, "
            f"true peak {after.true_peak:.4f} ({20 * math.log10(after.true_peak) if after.true_peak else -240:.2f} dBTP)")
    return after


def _intermediate_path(destination: Path) -> Path:
    """A sibling temp path so the final rename stays on one filesystem."""

    destination = Path(destination)
    try:
        destination.parent.mkdir(parents=True, exist_ok=True)
        fd, name = tempfile.mkstemp(prefix=f".{destination.name}.stage.",
                                    suffix=".wav", dir=str(destination.parent))
        os.close(fd)
        Path(name).unlink(missing_ok=True)
        return Path(name)
    except OSError as exc:
        raise WriteError(f"cannot create staging file for {destination}: {exc}",
                         details={"path": str(destination)}) from exc


def _unlink(path: Path) -> None:
    try:
        Path(path).unlink(missing_ok=True)
    except OSError:
        pass
