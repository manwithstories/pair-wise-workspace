"""Block processing primitives used by the single offline/streaming executor."""

from __future__ import annotations

import numpy as np

from .automation import db_to_gain
from .errors import RenderError


def _audio(block: np.ndarray) -> np.ndarray:
    value = np.asarray(block, dtype=np.float32)
    if value.ndim == 1:
        value = value[None, :]
    if value.ndim != 2:
        raise RenderError("audio blocks must be channel-first 2-D arrays")
    return value


def read_track_block(
    handle: object,
    start: int,
    stop: int,
    channels: int,
    *,
    track_id: str = "",
) -> np.ndarray:
    """Read a channel-first frame range, padding short material with zeros."""
    if start < 0 or stop < start:
        raise RenderError("invalid read range", details={"start": start, "stop": stop})
    if start >= int(handle.frames):
        return np.zeros((channels, stop - start), dtype=np.float32)
    handle.seek(start)
    wanted = stop - start
    raw = handle.read(wanted, dtype="float32", always_2d=True) if wanted else np.zeros((0, channels))
    audio = np.asarray(raw, dtype=np.float32).T
    if audio.shape[0] != channels:
        raise RenderError(
            "track channel count changed while reading",
            details={"track_id": track_id, "expected": channels, "actual": audio.shape[0]},
        )
    if audio.size and not np.all(np.isfinite(audio)):
        raise RenderError(
            "track contains NaN or Inf samples",
            details={"track_id": track_id},
        )
    if audio.shape[1] < wanted:
        audio = np.pad(audio, ((0, 0), (0, wanted - audio.shape[1])))
    return audio


def gain_db_block(block: np.ndarray, gain_db: np.ndarray | float) -> np.ndarray:
    audio = _audio(block)
    gains = np.asarray(db_to_gain(gain_db), dtype=np.float32)
    if gains.ndim == 0:
        return audio * gains
    if gains.shape != (audio.shape[1],):
        raise RenderError("per-sample gain shape does not match audio")
    return audio * gains[None, :]


def pan_block(block: np.ndarray, pan: float, output_channels: int) -> np.ndarray:
    """Constant-power mono pan; linear balance for stereo input."""
    audio = _audio(block)
    pan = float(pan)
    if not -1.0 <= pan <= 1.0:
        raise RenderError("pan must be in [-1, 1]", details={"pan": pan})
    if output_channels not in (1, 2):
        raise RenderError("output channels must be 1 or 2")
    if output_channels == 1:
        if audio.shape[0] == 1:
            return audio
        return audio.mean(axis=0, keepdims=True).astype(np.float32)
    if audio.shape[0] == 1:
        angle = (pan + 1.0) * np.pi / 4.0
        left, right = np.cos(angle), np.sin(angle)
        return np.vstack((audio[0] * left, audio[0] * right)).astype(np.float32)
    if audio.shape[0] != 2:
        raise RenderError(
            "pan input must be mono or stereo",
            details={"channels": int(audio.shape[0])},
        )
    left_gain = 1.0 if pan <= 0.0 else 1.0 - pan
    right_gain = 1.0 if pan >= 0.0 else 1.0 + pan
    return np.vstack((audio[0] * left_gain, audio[1] * right_gain)).astype(np.float32)


def sum_blocks(blocks: list[np.ndarray] | tuple[np.ndarray, ...], channels: int) -> np.ndarray:
    if not blocks:
        raise RenderError("sum node requires at least one input")
    lengths = [np.asarray(block).shape[-1] for block in blocks]
    if len(set(lengths)) != 1:
        raise RenderError("sum node received blocks with different lengths", details={"lengths": lengths})
    result = np.zeros((channels, lengths[0]), dtype=np.float32)
    for block in blocks:
        audio = _audio(block)
        if audio.shape[0] == 1 and channels == 2:
            result += audio
        elif audio.shape[0] != channels:
            raise RenderError(
                "sum input channel layout does not match project channels",
                details={"expected": channels, "actual": int(audio.shape[0])},
            )
        else:
            result += audio
    return result


def trim_block(block: np.ndarray, max_samples: int | None, start: int = 0) -> np.ndarray:
    audio = _audio(block)
    if start < 0:
        raise RenderError("trim start must be non-negative")
    if max_samples is None:
        return audio
    if max_samples < 0:
        raise RenderError("trim max_samples must be non-negative")
    return audio[:, start : start + max_samples]


def soft_limit_block(block: np.ndarray, ceiling: float = 1.0, softness: float = 1.0) -> np.ndarray:
    """Unity-slope soft saturation with an output strictly below ceiling."""
    audio = _audio(block)
    if not 0.0 < ceiling <= 1.0:
        raise RenderError("limiter ceiling must be in (0, 1]", details={"ceiling": ceiling})
    if softness < 1.0:
        raise RenderError("limiter softness must be >= 1", details={"softness": softness})
    return (ceiling * np.tanh(np.asarray(audio, dtype=np.float32) * (softness / ceiling)) / softness).astype(
        np.float32
    )
