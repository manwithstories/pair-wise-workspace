"""Immutable domain data structures shared across the engine.

Every dataclass is frozen (and slot-based) so decision history for very
large traces stays cheap and cannot be mutated after construction.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from enum import Enum


class ABRError(Exception):
    """Base class for all engine errors (exit code 2)."""


class ABRConfigError(ABRError):
    """Illegal CLI argument, model parameter or bitrate ladder."""


class ABRTraceError(ABRError):
    """Illegal trace content (empty, duplicate timestamps, bad samples)."""


class ABRSimulationError(Exception):
    """Unexpected failure while running a simulation (exit code 3)."""


class EstimatorState(str, Enum):
    """Working state reported by the bandwidth estimator."""

    WARMUP = "warmup"          # not enough samples yet
    OK = "ok"                  # confident weighted estimate
    ZERO = "zero"              # all recent throughput samples are zero
    REGRESSION = "regression"  # a non-monotonic timestamp was observed
    EMPTY = "empty"            # no samples at all

    @property
    def safe(self) -> bool:
        """True when the estimate is usable for an upshift decision."""
        return self is EstimatorState.OK


def _finite(value: float, name: str) -> float:
    value = float(value)
    if not math.isfinite(value):
        raise ABRConfigError(f"{name} must be a finite number, got {value!r}")
    return value


@dataclass(frozen=True, slots=True)
class BitrateLadder:
    """Ordered, strictly ascending set of selectable bitrates (kbps)."""

    bitrates: tuple[int, ...]

    def __post_init__(self) -> None:
        if not self.bitrates:
            raise ABRConfigError("bitrate ladder must contain at least one level")
        prev = None
        for level in self.bitrates:
            if not isinstance(level, int) or isinstance(level, bool):
                raise ABRConfigError(f"bitrate levels must be integers, got {level!r}")
            if level <= 0:
                raise ABRConfigError(f"bitrate levels must be positive, got {level}")
            if prev is not None and level <= prev:
                raise ABRConfigError(
                    "bitrate ladder must be strictly ascending, "
                    f"got {prev} followed by {level}"
                )
            prev = level

    @classmethod
    def from_kbps(cls, values: list[int] | tuple[int, ...]) -> "BitrateLadder":
        return cls(tuple(int(v) for v in values))

    @property
    def size(self) -> int:
        return len(self.bitrates)

    @property
    def lowest(self) -> int:
        return self.bitrates[0]

    @property
    def highest(self) -> int:
        return self.bitrates[-1]

    def clamp(self, index: int) -> int:
        """Constrain a level index to the valid range."""
        if index < 0:
            return 0
        if index >= self.size:
            return self.size - 1
        return index

    def highest_affordable(self, bandwidth_kbps: float) -> int:
        """Highest level whose bitrate fits inside ``bandwidth_kbps``."""
        chosen = 0
        for index, level in enumerate(self.bitrates):
            if level <= bandwidth_kbps:
                chosen = index
            else:
                break
        return chosen


@dataclass(frozen=True, slots=True)
class NetworkSample:
    """One observed network window: throughput in kbps, rtt in ms, loss 0..1."""

    t: float           # timestamp, seconds, must be strictly increasing
    throughput: float  # observed throughput, kbps; zero means a dead link
    rtt: float         # round-trip time, milliseconds
    loss: float        # packet loss ratio in [0, 1]

    def __post_init__(self) -> None:
        t = _finite(self.t, "timestamp")
        throughput = _finite(self.throughput, "throughput")
        rtt = _finite(self.rtt, "rtt")
        loss = _finite(self.loss, "loss")
        if throughput < 0:
            raise ABRTraceError(f"throughput must be >= 0, got {throughput}")
        if rtt < 0:
            raise ABRTraceError(f"rtt must be >= 0, got {rtt}")
        if loss < 0:
            raise ABRTraceError(f"packet loss must be >= 0, got {loss}")
        if loss > 1:
            raise ABRTraceError(f"packet loss must be <= 1, got {loss}")
        object.__setattr__(self, "t", t)
        object.__setattr__(self, "throughput", throughput)
        object.__setattr__(self, "rtt", rtt)
        object.__setattr__(self, "loss", loss)


@dataclass(frozen=True, slots=True)
class Trace:
    """A full network trace plus the generation metadata behind it."""

    samples: tuple[NetworkSample, ...]
    segment_seconds: float
    seed: int | None = None
    name: str = "trace"

    def __post_init__(self) -> None:
        if not self.samples:
            raise ABRTraceError("trace must contain at least one segment")
        if self.segment_seconds <= 0 or not math.isfinite(self.segment_seconds):
            raise ABRConfigError(
                f"segment-seconds must be positive, got {self.segment_seconds}"
            )
        # Hard upper bound rejects the "oversized segment" failure mode.
        if self.segment_seconds > 86_400.0:
            raise ABRConfigError(
                f"segment-seconds {self.segment_seconds} exceeds the 86400s limit"
            )
        last: float | None = None
        for sample in self.samples:
            if last is not None and sample.t == last:
                raise ABRTraceError(
                    f"duplicate timestamp {sample.t} in trace; timestamps must be unique"
                )
            last = sample.t

    def __len__(self) -> int:
        return len(self.samples)


@dataclass(frozen=True, slots=True)
class Estimate:
    """Output of the bandwidth estimator for one decision point."""

    bandwidth_kbps: float
    state: EstimatorState
    samples_used: int
    loss: float
    rtt: float


@dataclass(frozen=True, slots=True)
class BufferState:
    """Player buffer snapshot before a segment is downloaded."""

    level_seconds: float
    max_seconds: float

    def __post_init__(self) -> None:
        if self.level_seconds < 0 or not math.isfinite(self.level_seconds):
            raise ABRConfigError(f"buffer level must be >= 0, got {self.level_seconds}")
        if self.max_seconds <= 0:
            raise ABRConfigError(f"max buffer must be > 0, got {self.max_seconds}")

    @property
    def ratio(self) -> float:
        return min(1.0, self.level_seconds / self.max_seconds)


@dataclass(frozen=True, slots=True)
class Decision:
    """Per-segment decision record — exactly the fields the report requires."""

    index: int
    bitrate_kbps: int
    est_bandwidth_kbps: float
    estimator_state: EstimatorState
    buffer_seconds: float
    download_seconds: float
    switch: bool
    stall: bool
    stall_seconds: float
    throughput_kbps: float


@dataclass(frozen=True, slots=True)
class SimulationConfig:
    """All tunables for one simulation run."""

    bitrates: tuple[int, ...]
    segment_seconds: float
    initial_buffer: float
    max_buffer: float
    window: int
    upshift_confirm: int
    downshift_confirm: int = 1
    safety_margin: float = 0.85
    stall_cap_factor: float = 2.0
    seed: int | None = None
    extras: dict = field(default_factory=dict)

    def __post_init__(self) -> None:
        BitrateLadder(self.bitrates)  # validates the ladder
        if self.segment_seconds <= 0 or not math.isfinite(self.segment_seconds):
            raise ABRConfigError("segment-seconds must be positive")
        if self.segment_seconds > 86_400.0:
            raise ABRConfigError("segment-seconds exceeds the 86400s limit")
        if self.initial_buffer < 0:
            raise ABRConfigError("initial-buffer must be >= 0")
        if self.max_buffer <= 0:
            raise ABRConfigError("max-buffer must be > 0")
        if self.initial_buffer > self.max_buffer:
            raise ABRConfigError("initial-buffer cannot exceed max-buffer")
        if self.window < 1:
            raise ABRConfigError("window must be >= 1")
        if self.upshift_confirm < 1:
            raise ABRConfigError("upshift-confirm must be >= 1")
        if self.downshift_confirm < 1:
            raise ABRConfigError("downshift-confirm must be >= 1")
        if not 0.0 < self.safety_margin <= 1.0:
            raise ABRConfigError("safety-margin must be in (0, 1]")
        if self.stall_cap_factor <= 0:
            raise ABRConfigError("stall-cap-factor must be > 0")

    @property
    def ladder(self) -> BitrateLadder:
        return BitrateLadder(self.bitrates)
