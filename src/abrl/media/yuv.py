"""YUV420p planar file IO, frame iteration and resolution scaling."""
from __future__ import annotations

import hashlib
from dataclasses import dataclass
from pathlib import Path

import numpy as np

DTYPE = np.uint8


@dataclass(frozen=True)
class Frame:
    """One YUV420p frame: luma and the two half-resolution chroma planes."""

    y: np.ndarray
    u: np.ndarray
    v: np.ndarray

    @property
    def width(self) -> int:
        return int(self.y.shape[1])

    @property
    def height(self) -> int:
        return int(self.y.shape[0])


def plane_sizes(width: int, height: int) -> tuple[int, int, int]:
    """Byte counts of the Y, U and V planes for one YUV420p frame."""
    if width % 2 or height % 2:
        raise ValueError(f"YUV420p needs even dimensions, got {width}x{height}")
    return width * height, (width // 2) * (height // 2), (width // 2) * (height // 2)


def frame_size(width: int, height: int) -> int:
    return sum(plane_sizes(width, height))


def frame_count(path: str | Path, width: int, height: int) -> int:
    total = Path(path).stat().st_size
    size = frame_size(width, height)
    if total % size:
        raise ValueError(f"{path}: {total} bytes is not a multiple of frame size {size}")
    return total // size


def digest_file(path: str | Path, buf_size: int = 1 << 20) -> str:
    """SHA-256 over the raw source bytes; feeds the task key."""
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        for chunk in iter(lambda: fh.read(buf_size), b""):
            h.update(chunk)
    return h.hexdigest()


def _plane_from_bytes(buf: bytes, h: int, w: int) -> np.ndarray:
    return np.frombuffer(buf, dtype=DTYPE).reshape(h, w).copy()


def read_frame(buf: bytes, width: int, height: int) -> Frame:
    ny, nu, nv = plane_sizes(width, height)
    if len(buf) != ny + nu + nv:
        raise ValueError(f"buffer {len(buf)} != expected frame {ny + nu + nv}")
    y = _plane_from_bytes(buf[:ny], height, width)
    u = _plane_from_bytes(buf[ny:ny + nu], height // 2, width // 2)
    v = _plane_from_bytes(buf[ny + nu:], height // 2, width // 2)
    return Frame(y, u, v)


def iter_frames(path: str | Path, width: int, height: int):
    """Lazily yield :class:`Frame` objects from a planar YUV420p file."""
    size = frame_size(width, height)
    with open(path, "rb") as fh:
        while True:
            buf = fh.read(size)
            if not buf:
                break
            if len(buf) != size:
                raise ValueError(f"{path}: truncated trailing frame {len(buf)} bytes")
            yield read_frame(buf, width, height)


def read_all(path: str | Path, width: int, height: int) -> list[Frame]:
    return list(iter_frames(path, width, height))


def write_frames(path: str | Path, frames) -> int:
    """Write frames to ``path`` and return the byte count."""
    out = Path(path)
    out.parent.mkdir(parents=True, exist_ok=True)
    written = 0
    with open(out, "wb") as fh:
        for frame in frames:
            for plane in (frame.y, frame.u, frame.v):
                fh.write(np.ascontiguousarray(plane, dtype=DTYPE).tobytes())
                written += plane.size
    return written


def _resize_plane(plane: np.ndarray, new_h: int, new_w: int) -> np.ndarray:
    """Area/bilinear-ish resample using float coordinate center mapping."""
    h, w = plane.shape
    if (h, w) == (new_h, new_w):
        return plane.copy()
    src = plane.astype(np.float32)
    row_idx = ((np.arange(new_h, dtype=np.float32) + 0.5) * h / new_h - 0.5)
    col_idx = ((np.arange(new_w, dtype=np.float32) + 0.5) * w / new_w - 0.5)
    r0 = np.floor(row_idx).astype(np.int64)
    c0 = np.floor(col_idx).astype(np.int64)
    r1 = np.clip(r0 + 1, 0, h - 1)
    c1 = np.clip(c0 + 1, 0, w - 1)
    r0c = np.clip(r0, 0, h - 1)
    c0c = np.clip(c0, 0, w - 1)
    wr = (row_idx - np.clip(row_idx, 0, h - 1))[..., None]
    wc = (col_idx - np.clip(col_idx, 0, w - 1))[None, ...]
    top = src[r0c][:, c0c] * (1 - wc) + src[r0c][:, c1] * wc
    bot = src[r1][:, c0c] * (1 - wc) + src[r1][:, c1] * wc
    out = top * (1 - wr) + bot * wr
    return np.clip(np.rint(out), 0, 255).astype(DTYPE)


def resize(frame: Frame, new_width: int, new_height: int) -> Frame:
    """Resize a YUV420p frame; dimensions must remain even."""
    if new_width % 2 or new_height % 2:
        raise ValueError(f"target must be even, got {new_width}x{new_height}")
    y = _resize_plane(frame.y, new_height, new_width)
    u = _resize_plane(frame.u, new_height // 2, new_width // 2)
    v = _resize_plane(frame.v, new_height // 2, new_width // 2)
    return Frame(y, u, v)
