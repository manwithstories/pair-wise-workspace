"""Deterministic signal generation: pulses, footsteps, ambience and IRs.

Everything is derived from NumPy ``SeedSequence``/``Generator`` instances,
so a given seed always reproduces byte-identical signals. No external
files are referenced; all fixtures are generated here.
"""
from __future__ import annotations

import numpy as np

from .config import RenderConfig

PULSE_AMPLITUDE = 0.55
FOOTSTEP_AMPLITUDE = 0.40
AMBIENT_AMPLITUDE = 0.012
FOOTSTEP_COUNT = 5
_ATTACK_SAMPLES = 32


def _smooth_attack(envelope: np.ndarray) -> np.ndarray:
    """Half-cosine onset so the IR start does not click."""
    attack = min(_ATTACK_SAMPLES, envelope.shape[0])
    if attack <= 1:
        return envelope
    ramp = 0.5 - 0.5 * np.cos(np.pi * np.arange(attack) / attack)
    out = envelope.copy()
    out[:attack] *= ramp
    return out


def make_impulse_response(length: int, channels: int,
                          rng: np.random.Generator) -> np.ndarray:
    """Peak-normalised exponential-decay noise IR, shape ``(length, channels)``.

    Each channel is an independent noise realisation normalised to its own
    unit peak, so stereo responses are genuinely independent and the tail
    never becomes exactly silent (the decay is exponential).
    """
    if length <= 0:
        raise ValueError("impulse response length must be positive")
    if channels not in (1, 2):
        raise ValueError("channels must be 1 or 2")
    t = np.arange(length, dtype=np.float64)
    tau = max(length / 6.0, 1.0)
    envelope = _smooth_attack(np.exp(-t / tau))
    columns = []
    for _ in range(channels):
        noise = rng.standard_normal(length)
        ir = noise * envelope
        peak = float(np.max(np.abs(ir)))
        if peak == 0.0:  # pragma: no cover - impossible for normal noise
            raise ValueError("degenerate all-zero impulse response generated")
        columns.append(ir / peak)
    return np.stack(columns, axis=1).astype(np.float64, copy=False)


def make_input_signal(n_samples: int, channels: int,
                      rng: np.random.Generator) -> np.ndarray:
    """Deterministic source signal, shape ``(n_samples, channels)``.

    Combines an opening delta pulse, five Hann-windowed footstep bursts
    (each with independent per-channel draws) and low-level stereo
    ambient noise. An empty input returns a zero-row array.
    """
    if channels not in (1, 2):
        raise ValueError("channels must be 1 or 2")
    if n_samples <= 0:
        return np.zeros((0, channels), dtype=np.float64)

    x = np.zeros((n_samples, channels), dtype=np.float64)
    x[0, :] = PULSE_AMPLITUDE

    window_len = min(256, max(8, n_samples // 40))
    positions = np.linspace(
        int(n_samples * 0.12), int(n_samples * 0.88), FOOTSTEP_COUNT
    ).astype(int)
    hann = 0.5 - 0.5 * np.cos(
        2.0 * np.pi * np.arange(window_len) / max(window_len - 1, 1)
    )
    for i, pos in enumerate(positions):
        end = min(pos + window_len, n_samples)
        width = end - pos
        if width <= 0:
            continue
        decay = 0.9**i
        for ch in range(channels):
            burst = rng.standard_normal(width)
            x[pos:end, ch] += FOOTSTEP_AMPLITUDE * decay * hann[:width] * burst

    x += AMBIENT_AMPLITUDE * rng.standard_normal((n_samples, channels))
    return x


def build_scene(cfg: RenderConfig) -> dict:
    """Generate the reproducible input + IR pair for a render config."""
    seed_seq = np.random.SeedSequence(cfg.seed)
    input_rng, ir_rng = (np.random.default_rng(s) for s in seed_seq.spawn(2))
    source = make_input_signal(cfg.input_samples, cfg.channels, input_rng)
    ir = make_impulse_response(cfg.ir_length, cfg.channels, ir_rng)
    return {
        "input": source,
        "ir": ir,
        "sample_rate": cfg.sample_rate,
        "seed": cfg.seed,
    }


# ---------------------------------------------------------------------------
# Fixed fixtures for tests — every one is seeded and therefore reproducible.
# ---------------------------------------------------------------------------
def fixture_short_pulse():
    """Single-sample delta input and a short 4-tap IR."""
    rng = np.random.default_rng(101)
    x = np.zeros((1, 1), dtype=np.float64)
    x[0, 0] = 1.0
    ir = make_impulse_response(4, 1, rng)
    return x, ir


def fixture_long_tail_ir(block_size: int = 64):
    """IR much longer than one block; exercises multi-block IR handling."""
    rng = np.random.default_rng(202)
    x = make_input_signal(block_size * 2 + 13, 1, rng)
    ir = make_impulse_response(block_size * 5, 1,
                               np.random.default_rng(203))
    return x, ir


def fixture_partial_last_block(block_size: int = 32):
    """Input length deliberately not a multiple of block size."""
    rng = np.random.default_rng(303)
    x = make_input_signal(block_size * 2 + 7, 1, rng)
    ir = make_impulse_response(block_size + 5, 1,
                               np.random.default_rng(304))
    return x, ir


def fixture_stereo_independent():
    """Two clearly distinct IR channels proving independent processing."""
    rng = np.random.default_rng(404)
    x = make_input_signal(200, 2, rng)
    length = 97
    left = np.linspace(1.0, 0.1, length)                      # smooth ramp
    right = rng.standard_normal(length)                       # random noise
    right /= max(np.max(np.abs(right)), 1e-12)
    ir = np.stack([left, right], axis=1).astype(np.float64)
    return x, ir
