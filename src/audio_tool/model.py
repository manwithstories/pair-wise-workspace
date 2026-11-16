"""Domain model: audio blocks, channel layouts, processing config, statistics.

Dependency-free.  Sample data travels as ``array('d')`` plane-major groups (all
of channel 0, then channel 1, ...) so a block costs exactly
``8 * frames * channels`` bytes and needs no third-party array library.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from typing import Iterable, Sequence

MIN_SAMPLE_RATE = 8000
MAX_SAMPLE_RATE = 192000
MIN_TRUE_PEAK_DBTP = -20.0
MAX_TRUE_PEAK_DBTP = 0.0

#: Frames per processing block; bounds resident samples for 96 kHz 7.1 input.
DEFAULT_BLOCK_FRAMES = 16384

#: BS.1770-4 absolute gate (-70 LUFS) expressed as mean square.
ABSOLUTE_GATE = 10.0 ** ((-70.0 + 0.691) / 10.0)

#: BS.1770-4 relative gate: 10 dB below the ungated loudness.
RELATIVE_GATE_OFFSET = -10.0


class AudioToolError(Exception):
    """Base class for errors that map to a process exit code."""

    exit_code = 1
    code = "audio_tool_error"

    def __init__(self, message: str, *, code: str | None = None,
                 details: dict | None = None):
        super().__init__(message)
        self.message = message
        if code is not None:
            self.code = code
        self.details = details or {}


class UsageError(AudioToolError):
    """Invalid parameters or unavailable input (exit code 2)."""

    exit_code = 2
    code = "usage_error"


class WriteError(AudioToolError):
    """Output or report could not be written (exit code 3)."""

    exit_code = 3
    code = "write_error"


class InputFormatError(AudioToolError):
    """One input file is unusable; the batch continues (exit code 1)."""

    exit_code = 1
    code = "input_invalid"


class UnsupportedError(InputFormatError):
    """Well-formed but outside the supported subset (rate/layout/encoding)."""

    code = "unsupported_format"


class SilentMaterialError(InputFormatError):
    """Digital silence: gain is undefined, so the file is skipped."""

    code = "silent_material"


# ---------------------------------------------------------------------------
# Channel layouts
# ---------------------------------------------------------------------------

#: Channel-count -> canonical layout name.
CHANNEL_LAYOUTS: dict[int, str] = {
    1: "mono", 2: "stereo", 3: "2.1", 4: "quad",
    5: "5.0", 6: "5.1", 7: "6.1", 8: "7.1",
}

#: Per-channel BS.1770-4 weights.  LFE contributes 0; surround pairs use 1.41.
_WEIGHTS: dict[str, tuple[float, ...]] = {
    "mono": (1.0,),
    "stereo": (1.0, 1.0),
    "round21": (1.0, 1.0, 0.0),
    "quad": (1.0, 1.0, 1.41, 1.41),
    "round50": (1.0, 1.0, 1.0, 1.41, 1.41),
    "round51": (1.0, 1.0, 1.0, 0.0, 1.41, 1.41),
    "round61": (1.0, 1.0, 1.0, 0.0, 1.41, 1.41, 1.41),
    "round71": (1.0, 1.0, 1.0, 0.0, 1.41, 1.41, 1.41, 1.41),
}
_WEIGHT_KEY = {3: "round21", 4: "quad", 5: "round50",
               6: "round51", 7: "round61", 8: "round71"}


@dataclass(frozen=True)
class ChannelLayout:
    """A validated channel configuration with one BS.1770 weight per channel."""

    channels: int
    name: str
    weights: tuple[float, ...]

    @property
    def is_multichannel(self) -> bool:
        return self.channels > 2

    def describe(self) -> str:
        return f"{self.name} ({self.channels} ch)"


def resolve_layout(channels: int) -> ChannelLayout:
    """Build the layout descriptor for a channel count (1-8)."""

    if channels < 1:
        raise UsageError(f"channel count must be >= 1, got {channels}",
                         code="bad_channel_count")
    if channels in (1, 2):
        name = "mono" if channels == 1 else "stereo"
        return ChannelLayout(channels, name, (1.0,) * channels)
    if channels not in CHANNEL_LAYOUTS:
        raise UnsupportedError(
            f"unsupported channel count {channels}; supported: 1-8",
            details={"channels": channels})
    name = CHANNEL_LAYOUTS[channels]
    return ChannelLayout(channels, name, _WEIGHTS[_WEIGHT_KEY[channels]])


# ---------------------------------------------------------------------------
# Processing configuration
# ---------------------------------------------------------------------------


@dataclass(frozen=True)
class ResampleConfig:
    """Parameters of the self-contained polyphase resampling kernel.

    ``passband_hz`` is the low-pass cutoff; ``None`` derives one from the
    slower of the two rates (92% of its Nyquist).  ``half_taps`` is the
    one-sided windowed-sinc length (``2 * half_taps + 1`` total taps),
    ``phases`` the number of fractional delays quantised per input sample.
    """

    passband_hz: float | None = None
    half_taps: int = 48
    phases: int = 256
    kaiser_beta: float = 8.6

    def cutoff_for(self, in_rate: int, out_rate: int) -> float:
        """Effective cutoff in Hz, always below both Nyquist limits."""

        nyquist = 0.5 * min(in_rate, out_rate)
        return 0.92 * nyquist if self.passband_hz is None else float(self.passband_hz)

    def validate(self, in_rate: int | None = None, out_rate: int | None = None) -> None:
        if self.passband_hz is not None and self.passband_hz <= 0:
            raise UsageError("resampler passband must be > 0", code="bad_resampler")
        if self.passband_hz is not None and in_rate and out_rate:
            limit = 0.5 * min(in_rate, out_rate)
            if self.passband_hz >= limit:
                raise UsageError(
                    f"resampler passband {self.passband_hz:.0f} Hz must stay below the "
                    f"lower Nyquist ({limit:.0f} Hz)", code="bad_resampler")
        if self.half_taps < 4:
            raise UsageError("resampler window length must be >= 4 taps each side",
                             code="bad_resampler")
        if self.phases < 8:
            raise UsageError("resampler phase count must be >= 8", code="bad_resampler")
        if not (0.0 < self.kaiser_beta <= 20.0):
            raise UsageError("resampler kaiser beta must be in (0, 20]",
                             code="bad_resampler")

    def signature(self) -> dict:
        return {"passband_hz": self.passband_hz, "half_taps": self.half_taps,
                "phases": self.phases, "kaiser_beta": self.kaiser_beta}


@dataclass(frozen=True)
class ProcessConfig:
    """Everything needed to turn one input file into one output file."""

    target_rate: int
    target_lufs: float
    true_peak_db: float
    block_frames: int = DEFAULT_BLOCK_FRAMES
    resampler: ResampleConfig = field(default_factory=ResampleConfig)
    output_subtype: str | None = None
    output_format: str = "WAV"

    def validate(self) -> None:
        if not (MIN_SAMPLE_RATE <= self.target_rate <= MAX_SAMPLE_RATE):
            raise UsageError(
                f"target sample rate must be within {MIN_SAMPLE_RATE}.."
                f"{MAX_SAMPLE_RATE} Hz, got {self.target_rate}",
                code="bad_sample_rate")
        if not (MIN_TRUE_PEAK_DBTP <= self.true_peak_db <= MAX_TRUE_PEAK_DBTP):
            raise UsageError(
                f"true-peak ceiling must be within {MIN_TRUE_PEAK_DBTP}.."
                f"{MAX_TRUE_PEAK_DBTP} dBTP, got {self.true_peak_db}",
                code="bad_true_peak")
        if not (-70.0 <= self.target_lufs <= 0.0):
            raise UsageError(
                f"target loudness must be within -70..0 LUFS, got {self.target_lufs}",
                code="bad_target_lufs")
        if self.block_frames < 256:
            raise UsageError("block size must be >= 256 frames", code="bad_block_size")
        self.resampler.validate()

    def signature(self) -> dict:
        return {"target_rate": self.target_rate, "target_lufs": self.target_lufs,
                "true_peak_db": self.true_peak_db, "block_frames": self.block_frames,
                "resampler": self.resampler.signature(),
                "output_format": self.output_format,
                "output_subtype": self.output_subtype}


# ---------------------------------------------------------------------------
# Audio blocks and statistics
# ---------------------------------------------------------------------------


@dataclass
class AudioBlock:
    """A plane-major chunk of samples plus its true frame count.

    ``planes[c]`` holds at least ``frames`` doubles for channel ``c``.
    """

    planes: list[Sequence[float]]
    frames: int

    @property
    def channels(self) -> int:
        return len(self.planes)

    def iter_frames(self) -> Iterable[tuple[float, ...]]:
        for index in range(self.frames):
            yield tuple(plane[index] for plane in self.planes)


@dataclass
class LoudnessStats:
    """Measured loudness/level values for one pass over the material."""

    frames: int = 0
    peak: float = 0.0
    true_peak: float = 0.0
    rms: float = 0.0
    lufs: float | None = None
    momentary_max: float | None = None
    short_term_max: float | None = None
    clipped_samples: int = 0
    digital_silence: bool = False

    def to_dict(self) -> dict:
        return {
            "frames": self.frames,
            "peak": round(self.peak, 8),
            "true_peak": round(self.true_peak, 8),
            "true_peak_dbtp": round(linear_to_db(self.true_peak), 4),
            "rms": round(self.rms, 8),
            "rms_dbfs": round(linear_to_db(self.rms), 4),
            "lufs": None if self.lufs is None else round(self.lufs, 4),
            "momentary_max_lufs": None if self.momentary_max is None
            else round(self.momentary_max, 4),
            "short_term_max_lufs": None if self.short_term_max is None
            else round(self.short_term_max, 4),
            "clipped_samples": self.clipped_samples,
            "digital_silence": self.digital_silence,
        }


def linear_to_db(value: float) -> float:
    """20*log10 with a floor that keeps JSON finite for digital silence."""

    if value <= 1e-12:
        return -240.0
    return 20.0 * math.log10(value)
