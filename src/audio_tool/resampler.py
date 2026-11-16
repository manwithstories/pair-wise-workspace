"""Self-contained polyphase windowed-sinc resampling kernel.

No third-party resampler is used.  The prototype low-pass is a Kaiser-windowed
sinc, evaluated on a rational polyphase grid with
``up = out_rate // gcd`` and ``down = in_rate // gcd``:

    x_c(t) = sum_m x[m] * h(t - m),      h(t) = 2*fc*sinc(2*fc*t)*w(t)
    y[n]   = x_c(n * down / up)

``h`` has unit DC gain for any cutoff below Nyquist, so no gain compensation
is needed and the same coefficient bank serves every block.  One bank per
fractional phase turns the inner loop into ``sum(map(mul, coeffs, slice))``
(``sum``/``map``/slicing all run in C), which is what keeps pure-Python
throughput usable.

Streaming is frame-accurate: the stream holds a window of recent input and
emits every output whose tap window is fully covered, then trims history it
can never reach again.  The driver therefore feeds blocks and stays bounded
in memory.
"""

from __future__ import annotations

import math
import operator
from array import array
from typing import Sequence

from .dspmath import kaiser_window, sinc
from .io import AudioBlock
from .model import ResampleConfig, UsageError


def kaiser_beta_for(stopband_db: float) -> float:
    """Kaiser's empirical beta formula for a target stopband attenuation."""

    if stopband_db <= 21.0:
        return 0.0
    if stopband_db <= 50.0:
        return 0.5842 * (stopband_db - 21.0) ** 0.4 + 0.07886 * (stopband_db - 21.0)
    return 0.1102 * (stopband_db - 8.7)




class PolyphaseResampler:
    """Configuration + coefficient bank shared by every file in a batch.

    Construct once per batch (identical rates and config), then call
    :meth:`stream` per file.  All per-file state lives in the stream.
    """

    def __init__(self, config: ResampleConfig, in_rate: int, out_rate: int):
        if in_rate <= 0 or out_rate <= 0:
            raise UsageError("sample rates must be positive", code="bad_sample_rate")
        config.validate(in_rate, out_rate)
        self.config = config
        self.in_rate = int(in_rate)
        self.out_rate = int(out_rate)
        divisor = math.gcd(self.in_rate, self.out_rate)
        self.up = self.out_rate // divisor
        self.down = self.in_rate // divisor
        self.cutoff_hz = config.cutoff_for(self.in_rate, self.out_rate)
        if self.cutoff_hz >= 0.5 * min(self.in_rate, self.out_rate):
            raise UsageError(
                f"effective cutoff {self.cutoff_hz:.0f} Hz is at or above Nyquist",
                code="bad_resampler")
        self._fc = self.cutoff_hz / self.in_rate  # cycles per input sample
        self._half_taps = int(config.half_taps)
        self._beta = float(config.kaiser_beta)
        # The prototype's duration must be measured in TIME, not in input
        # samples: a 48-tap window is 1 ms at 48 kHz but only 0.25 ms at
        # 192 kHz, which would make the 3680 Hz low-pass of a 192->8 kHz
        # decimation almost an octave wide.  Scale the input-domain half
        # length by in/low so the window always spans `half_taps` periods of
        # the *slower* rate, where the transition band actually matters.
        low_rate = min(self.in_rate, self.out_rate)
        self._half = max(self._half_taps,
                         round(self._half_taps * self.in_rate / low_rate))
        self._taps = 2 * self._half + 1
        # Phase resolution finer than `up` cannot be resolved: a fractional
        # delay only ever needs `up` distinct values, so cap the bank size.
        self._phases = max(1, min(int(config.phases), max(self.up, 1)))
        self._bank = self._build_bank()

    @property
    def taps(self) -> int:
        return self._taps

    def _build_bank(self) -> list[tuple[float, ...]]:
        half, fc, beta = self._half, self._fc, self._beta
        banks: list[tuple[float, ...]] = []
        for phase in range(self._phases):
            frac = phase / self._phases
            coeffs = []
            for offset in range(-half, half + 1):
                t = offset - frac
                window = kaiser_window(t / half, beta)
                coeffs.append(2.0 * fc * sinc(2.0 * fc * t) * window)
            banks.append(tuple(coeffs))
        return banks

    def output_length(self, input_frames: int) -> int:
        """Exact output frame count for a finite input of N frames."""

        if input_frames <= 0:
            return 0
        return (input_frames * self.up + self.down - 1) // self.down

    def stream(self) -> "ResamplerStream":
        if self.in_rate == self.out_rate:
            return PassthroughStream(self)
        return ResamplerStream(self)

    def signature(self) -> dict:
        return {
            "in_rate": self.in_rate,
            "out_rate": self.out_rate,
            "up": self.up,
            "down": self.down,
            "cutoff_hz": round(self.cutoff_hz, 3),
            "taps": self._taps,
            "phases": self._phases,
            "kaiser_beta": self._beta,
        }


class ResamplerStream:
    """Per-file streaming state over a shared :class:`PolyphaseResampler`."""

    def __init__(self, kernel: PolyphaseResampler):
        self.kernel = kernel
        self._channels: int | None = None
        self._planes: list[array] = []
        self._hist_start = 0     # absolute input index of plane[0]
        self._hist_frames = 0
        self._total_in = 0
        self._emitted = 0
        self._pad = kernel._half + 1
        self._final = False

    # -- driving ------------------------------------------------------------

    def push(self, block: AudioBlock) -> list[AudioBlock]:
        """Feed one input block and return outputs whose windows are ready."""

        if self._final:
            raise RuntimeError("cannot push after flush()")
        if block.frames == 0:
            return []
        if self._channels is None:
            self._channels = block.channels
            self._planes = [array("d") for _ in range(block.channels)]
        elif block.channels != self._channels:
            raise UsageError("resampler stream saw a channel-count change",
                             code="bad_channel_count")
        for plane, incoming in zip(self._planes, block.planes):
            plane.extend(incoming[: block.frames])
        self._hist_frames += block.frames
        self._total_in += block.frames
        out = self._drain(final=False)
        return [out] if out is not None and out.frames else []

    def flush(self) -> list[AudioBlock]:
        """Zero-pad the tail, emit the remaining outputs, and finish."""

        if self._channels is None or self._final:
            return []
        self._final = True
        zeros = array("d", bytes(8 * self._pad))
        for plane in self._planes:
            plane.extend(zeros)
        self._hist_frames += self._pad
        out = self._drain(final=True)
        return [out] if out is not None and out.frames else []

    # -- internals ----------------------------------------------------------

    def _drain(self, final: bool) -> AudioBlock | None:
        kernel = self.kernel
        half, up, down = kernel._half, kernel.up, kernel.down
        channels = self._channels or 0
        if final:
            limit = kernel.output_length(self._total_in)
        else:
            # Largest output whose rightmost tap still lands inside history.
            reachable = self._hist_start + self._hist_frames - 1 - half
            limit = 0 if reachable < 0 else ((reachable * up) // down) + 1
        count = limit - self._emitted
        if count <= 0:
            return None
        bank, phases = kernel._bank, kernel._phases
        taps = kernel._taps
        out_planes = [array("d", bytes(8 * count)) for _ in range(channels)]
        planes = self._planes
        hist_start = self._hist_start
        hist_end = hist_start + self._hist_frames

        # Walk the output clock incrementally instead of dividing per sample:
        # `base`/`rem` hold floor(n*down/up) and the remainder, advanced by
        # `down` each step.  Saves two integer divisions per output sample,
        # which is a measurable share of pure-Python DSP time.
        numerator = self._emitted * down
        base, rem = divmod(numerator, up)
        for offset in range(count):
            phase = (rem * phases) // up
            coeffs = bank[phase]
            win_lo = base - half
            lo = win_lo if win_lo > hist_start else hist_start
            win_hi = win_lo + taps
            hi = win_hi if win_hi < hist_end else hist_end
            if hi <= lo:
                pass  # window lies entirely outside retained history: zeros
            elif lo == win_lo and hi == win_hi:
                # Interior fast path: a full window of real samples, no pad.
                start = lo - hist_start
                end = start + taps
                for chan in range(channels):
                    out_planes[chan][offset] = sum(
                        map(operator.mul, coeffs, planes[chan][start:end]))
            else:
                left = lo - win_lo
                body_len = hi - lo
                tail_pad = taps - left - body_len
                for chan in range(channels):
                    piece = list(planes[chan][lo - hist_start:hi - hist_start])
                    if left:
                        piece = [0.0] * left + piece
                    if tail_pad:
                        piece.extend([0.0] * tail_pad)
                    out_planes[chan][offset] = sum(map(operator.mul, coeffs, piece))
            rem += down
            base += rem // up
            rem %= up
        self._emitted = limit
        self._trim()
        return AudioBlock(planes=out_planes, frames=count)

    def _trim(self) -> None:
        """Drop history frames that no future output window can reach."""

        kernel = self.kernel
        half, up, down = kernel._half, kernel.up, kernel.down
        next_base = (self._emitted * down) // up
        keep_from = next_base - half
        drop = keep_from - self._hist_start
        if drop <= 0:
            return
        drop = min(drop, self._hist_frames)
        for plane in self._planes:
            del plane[:drop]
        self._hist_start += drop
        self._hist_frames -= drop


class PassthroughStream:
    """Bit-exact no-op used when the input already matches the target rate.

    Running identity through the convolution would attenuate the top of the
    band slightly (the prototype's transition band starts below Nyquist), so
    the natural case of "already at the delivery rate" is handled by copying.
    """

    def __init__(self, kernel: PolyphaseResampler):
        self.kernel = kernel

    def push(self, block: AudioBlock) -> list[AudioBlock]:
        if block.frames == 0:
            return []
        return [AudioBlock(planes=[array("d", plane[:block.frames])
                                   for plane in block.planes],
                           frames=block.frames)]

    def flush(self) -> list[AudioBlock]:
        return []
