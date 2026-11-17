"""Configuration model, validation rules, memory guard and exit codes.

All user-facing parameters are validated here before any signal is
generated or any FFT workspace is allocated, so that bad input fails
fast with an explainable error instead of a NumPy traceback.
"""
from __future__ import annotations

from dataclasses import dataclass

# ---------------------------------------------------------------------------
# Exit codes (single source of truth for the whole package)
# ---------------------------------------------------------------------------
EXIT_OK = 0
EXIT_PARAM_ERROR = 2
EXIT_NUMERIC_OR_MEMORY = 3
EXIT_PATH_OR_TAIL_MISMATCH = 4

# ---------------------------------------------------------------------------
# Hard sanity caps. These reject absurd dimensions before the memory guard
# even runs; ordinary reverb configurations are orders of magnitude smaller.
# ---------------------------------------------------------------------------
MAX_SAMPLE_RATE = 768_000
MAX_CHANNELS = 2                      # only mono / stereo are supported
MAX_BLOCK_SIZE = 1 << 20              # 1,048,576 samples per block
MAX_IR_LENGTH = 1 << 24               # ~16.7M-sample impulse response
MAX_DURATION_S = 3_600.0
MAX_SEED = 2**32 - 1

# Two complex128 FFT arrays plus real overlap/buffers, with headroom.
DEFAULT_MEMORY_BUDGET_BYTES = 2 * 1024**3

# Numeric agreement required between the offline and streaming paths.
PATH_MAX_ABS_ERROR_TOLERANCE = 1e-6


class ConfigError(ValueError):
    """Invalid user parameter (maps to exit code 2)."""


class MemoryGuardError(MemoryError):
    """Configuration would exceed the memory budget (maps to exit code 3)."""


class NumericConstraintError(RuntimeError):
    """NaN/Inf or other numeric constraint violated (maps to exit code 3)."""


class PathMismatchError(RuntimeError):
    """Offline/streaming disagreement or wrong tail length (exit code 4)."""


class StreamBlockError(RuntimeError):
    """A streaming block failed; writing must stop (maps to exit code 3).

    ``details`` carries a structured, JSON-serialisable description of
    where the failure happened and how many samples had been written.
    """

    def __init__(self, message: str, details: dict):
        super().__init__(message)
        self.details = details


@dataclass(frozen=True)
class RenderConfig:
    sample_rate: int
    channels: int
    block_size: int
    ir_length: int
    duration: float
    seed: int
    memory_budget_bytes: int = DEFAULT_MEMORY_BUDGET_BYTES

    @property
    def input_samples(self) -> int:
        """Number of source samples implied by duration and sample rate."""
        return int(round(self.duration * self.sample_rate))

    @property
    def output_samples(self) -> int:
        """Linear convolution length: N + M - 1."""
        return self.input_samples + self.ir_length - 1


def fft_size(block_size: int, ir_length: int) -> int:
    """Smallest power-of-two FFT that fits one block plus the full IR.

    Required size is ``B + M - 1`` (no circular wrap for overlap-add).
    """
    needed = block_size + ir_length - 1
    return 1 << (needed - 1).bit_length()


def estimate_workspace_bytes(cfg: RenderConfig) -> int:
    """Conservative upper bound on the working memory of one convolution.

    Covers the IR spectrum, per-block FFT inputs/outputs (complex128),
    the real overlap buffer, and the input/output float64 arrays of
    both render paths. A 2x safety factor absorbs temporaries inside
    NumPy's FFT and array concatenation.
    """
    n = cfg.input_samples
    m = cfg.ir_length
    k = fft_size(cfg.block_size, m)
    c = cfg.channels
    complex_workspace = 3 * k * c * 16          # H + block FFT + product
    real_workspace = k * c * 8                  # overlap tail
    buffers = (2 * n + m) * c * 8               # inputs + two outputs
    return 2 * (complex_workspace + real_workspace + buffers)


def validate_render_config(cfg: RenderConfig) -> None:
    """Validate every scalar rule; raise ConfigError / MemoryGuardError."""
    if not isinstance(cfg.sample_rate, int) or cfg.sample_rate <= 0:
        raise ConfigError("sample-rate must be a positive integer")
    if cfg.sample_rate > MAX_SAMPLE_RATE:
        raise ConfigError(f"sample-rate exceeds sanity cap {MAX_SAMPLE_RATE}")

    if not isinstance(cfg.channels, int) or cfg.channels not in (1, 2):
        raise ConfigError("channels must be 1 (mono) or 2 (stereo)")

    if not isinstance(cfg.block_size, int) or cfg.block_size <= 0:
        raise ConfigError("block-size must be a positive integer")
    if cfg.block_size > MAX_BLOCK_SIZE:
        raise ConfigError(f"block-size exceeds sanity cap {MAX_BLOCK_SIZE}")

    if not isinstance(cfg.ir_length, int) or cfg.ir_length <= 0:
        raise ConfigError(
            "ir-length must be a positive integer; a zero-length impulse "
            "response has no defined convolution output"
        )
    if cfg.ir_length > MAX_IR_LENGTH:
        raise ConfigError(f"ir-length exceeds sanity cap {MAX_IR_LENGTH}")

    if not isinstance(cfg.duration, (int, float)) or cfg.duration <= 0:
        raise ConfigError("duration must be a positive number of seconds")
    if cfg.duration > MAX_DURATION_S:
        raise ConfigError(f"duration exceeds sanity cap {MAX_DURATION_S}s")
    if cfg.input_samples <= 0:
        raise ConfigError("duration * sample-rate must yield at least one sample")

    if not isinstance(cfg.seed, int) or not (0 <= cfg.seed <= MAX_SEED):
        raise ConfigError(f"seed must be an integer in [0, {MAX_SEED}]")

    if cfg.memory_budget_bytes <= 0:
        raise ConfigError("memory budget must be positive")

    required = estimate_workspace_bytes(cfg)
    if required > cfg.memory_budget_bytes:
        raise MemoryGuardError(
            f"configuration needs ~{required:,} bytes of FFT workspace, "
            f"above the memory budget of {cfg.memory_budget_bytes:,} bytes; "
            "reduce block-size, ir-length or duration"
        )
