"""A small intra-frame DCT codec backed by zlib."""

from __future__ import annotations

import struct
import zlib
from functools import lru_cache

import numpy as np


MAGIC = b"ABR1"
MODES = {"420": 0, "444": 1, "gray": 2}
MODE_NAMES = {value: key for key, value in MODES.items()}
HEADER = struct.Struct(">4sBBBBHHH")


class CodecError(ValueError):
    """Raised when an encoded payload is malformed."""


@lru_cache(maxsize=3)
def _dct_matrix(size: int) -> np.ndarray:
    n = np.arange(size, dtype=np.float64)
    k = np.arange(size, dtype=np.float64)[:, None]
    matrix = np.cos(np.pi * (2.0 * n[None, :] + 1.0) * k / (2.0 * size))
    matrix[0, :] *= np.sqrt(1.0 / size)
    matrix[1:, :] *= np.sqrt(2.0 / size)
    return matrix.astype(np.float32)


def quant_step(qp: int) -> float:
    return float(2.0 ** ((int(qp) - 2) / 6.0))


def _rgb_to_planes(rgb: np.ndarray, mode: str) -> list[np.ndarray]:
    values = rgb.astype(np.float32)
    red, green, blue = values[..., 0], values[..., 1], values[..., 2]
    y = 0.299 * red + 0.587 * green + 0.114 * blue
    planes = [np.clip(np.rint(y), 0, 255)]
    if mode != "gray":
        u = -0.168736 * red - 0.331264 * green + 0.5 * blue + 128.0
        v = 0.5 * red - 0.418688 * green - 0.081312 * blue + 128.0
        if mode == "420":
            u = _downsample_420(u)
            v = _downsample_420(v)
        planes.extend([np.clip(np.rint(u), 0, 255), np.clip(np.rint(v), 0, 255)])
    return planes


def _downsample_420(plane: np.ndarray) -> np.ndarray:
    height, width = plane.shape
    padded = np.pad(plane, ((0, height % 2), (0, width % 2)), mode="edge")
    return padded.reshape(height // 2 + height % 2, 2, width // 2 + width % 2, 2).mean(axis=(1, 3))


def _upsample_420(plane: np.ndarray, height: int, width: int) -> np.ndarray:
    return np.repeat(np.repeat(plane, 2, axis=0), 2, axis=1)[:height, :width]


def _planes_to_rgb(planes: list[np.ndarray], mode: str, height: int, width: int) -> np.ndarray:
    y = planes[0]
    if mode == "gray":
        gray = np.clip(np.rint(y), 0, 255).astype(np.uint8)
        return np.repeat(gray[:, :, None], 3, axis=2)
    u = planes[1]
    v = planes[2]
    if mode == "420":
        u = _upsample_420(u, height, width)
        v = _upsample_420(v, height, width)
    u = u - 128.0
    v = v - 128.0
    red = y + 1.402 * v
    green = y - 0.344136 * u - 0.714136 * v
    blue = y + 1.772 * u
    rgb = np.stack([red, green, blue], axis=-1)
    return np.clip(np.rint(rgb), 0, 255).astype(np.uint8)


def _plane_shape(height: int, width: int, mode: str, channel: int) -> tuple[int, int]:
    if mode == "420" and channel > 0:
        return (height + 1) // 2, (width + 1) // 2
    return height, width


def _encode_plane(plane: np.ndarray, block_size: int, step: float, matrix: np.ndarray) -> np.ndarray:
    height, width = plane.shape
    pad_h = (-height) % block_size
    pad_w = (-width) % block_size
    padded = np.pad(plane.astype(np.float32), ((0, pad_h), (0, pad_w)), mode="edge")
    blocks = padded.reshape(height // block_size + bool(pad_h), block_size,
                            width // block_size + bool(pad_w), block_size)
    blocks = blocks.transpose(0, 2, 1, 3).reshape(-1, block_size, block_size)
    coefficients = matrix @ blocks @ matrix.T
    quantized = np.rint(coefficients / step)
    if np.abs(quantized).max(initial=0) > 32767:
        raise CodecError("quantized coefficient exceeds int16 range")
    return quantized.astype("<i2")


def _decode_plane(values: np.ndarray, height: int, width: int, block_size: int,
                  step: float, matrix: np.ndarray) -> np.ndarray:
    blocks_h = height // block_size + bool(height % block_size)
    blocks_w = width // block_size + bool(width % block_size)
    blocks = values.astype(np.float32).reshape(
        blocks_h, blocks_w, block_size, block_size
    ).reshape(-1, block_size, block_size)
    coefficients = blocks * step
    reconstructed = matrix.T @ coefficients @ matrix
    reconstructed = reconstructed.reshape(blocks_h, blocks_w, block_size, block_size)
    reconstructed = reconstructed.transpose(0, 2, 1, 3).reshape(
        blocks_h * block_size, blocks_w * block_size
    )
    return reconstructed[:height, :width]


def encode(rgb: np.ndarray, qp: int, block_size: int, chroma_mode: str) -> bytes:
    if rgb.ndim != 4 or rgb.shape[-1] != 3 or rgb.dtype != np.uint8:
        raise CodecError("frames must be uint8 RGB with shape (frames, height, width, 3)")
    if chroma_mode not in MODES:
        raise CodecError(f"unsupported chroma mode {chroma_mode!r}")
    if block_size not in (4, 8, 16):
        raise CodecError("block_size must be 4, 8 or 16")
    if not 1 <= qp <= 51:
        raise CodecError("qp must be between 1 and 51")
    frames, height, width, _ = rgb.shape
    if frames > 65535 or height > 65535 or width > 65535:
        raise CodecError("frame dimensions exceed payload limits")
    matrix = _dct_matrix(block_size)
    step = quant_step(qp)
    encoded_planes: list[np.ndarray] = []
    for frame in rgb:
        for plane in _rgb_to_planes(frame, chroma_mode):
            encoded_planes.append(_encode_plane(plane, block_size, step, matrix))
    body = b"".join(plane.tobytes(order="C") for plane in encoded_planes)
    header = HEADER.pack(MAGIC, MODES[chroma_mode], block_size, qp, 0, frames, height, width)
    return header + zlib.compress(body, level=6)


def decode(payload: bytes) -> np.ndarray:
    if len(payload) < HEADER.size:
        raise CodecError("payload is shorter than the header")
    try:
        magic, mode_id, block_size, qp, _reserved, frames, height, width = HEADER.unpack_from(payload)
    except struct.error as exc:
        raise CodecError(f"invalid payload header: {exc}") from exc
    if magic != MAGIC or mode_id not in MODE_NAMES:
        raise CodecError("unsupported payload header")
    if block_size not in (4, 8, 16) or not 1 <= qp <= 51:
        raise CodecError("invalid codec parameters in payload")
    if frames < 1 or height < 1 or width < 1:
        raise CodecError("invalid dimensions in payload")
    try:
        body = zlib.decompress(payload[HEADER.size:])
    except zlib.error as exc:
        raise CodecError(f"invalid entropy stream: {exc}") from exc
    mode = MODE_NAMES[mode_id]
    matrix = _dct_matrix(block_size)
    step = quant_step(qp)
    offset = 0
    output = np.empty((frames, height, width, 3), dtype=np.uint8)
    for frame_index in range(frames):
        planes: list[np.ndarray] = []
        for channel in range(1 if mode == "gray" else 3):
            plane_h, plane_w = _plane_shape(height, width, mode, channel)
            blocks_h = plane_h // block_size + bool(plane_h % block_size)
            blocks_w = plane_w // block_size + bool(plane_w % block_size)
            count = blocks_h * blocks_w * block_size * block_size
            end = offset + count * 2
            if end > len(body):
                raise CodecError("entropy stream ended early")
            values = np.frombuffer(body, dtype="<i2", count=count, offset=offset)
            planes.append(_decode_plane(values, plane_h, plane_w, block_size, step, matrix))
            offset = end
        output[frame_index] = _planes_to_rgb(planes, mode, height, width)
    if offset != len(body):
        raise CodecError("entropy stream has trailing coefficient data")
    return output
