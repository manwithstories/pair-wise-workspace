"""Pure-Python block transform codec with real bitstream IO.

The encoder writes a self-describing container (magic + header + per-frame
zlib/RLE payloads) to disk; bitrate is always measured from the resulting
file size, never estimated. Decoding reconstructs YUV420p frames used by the
quality metrics.
"""
from __future__ import annotations

import os
import struct
import zlib
from dataclasses import dataclass
from pathlib import Path

import numpy as np

from ..media.yuv import Frame, resize
from .dct import (
    BLOCK,
    dct_blocks,
    dequantize,
    idct_blocks,
    quant_table,
    quantize,
    zigzag_scan,
    zigzag_unscan,
)

MAGIC = b"ABRLCODE"
VERSION = 1
HEADER = struct.Struct(">8sHHHBB")  # magic,width,height,frames,fps,gop
FRAME_HDR = struct.Struct(">BII")  # type, payload_len, payload_crc32
PLANE_COUNT = 3


@dataclass(frozen=True)
class EncodeParams:
    width: int
    height: int
    fps: int
    qp: int
    gop: int

    def validate(self) -> None:
        if self.width % (BLOCK * 2) or self.height % (BLOCK * 2):
            raise ValueError(
                f"YUV420 dimensions must be multiples of {BLOCK * 2} "
                f"so chroma planes split into {BLOCK}x{BLOCK} blocks"
            )
        if self.fps <= 0 or self.gop <= 0:
            raise ValueError("fps and gop must be positive")
        if not 1 <= self.qp <= 51:
            raise ValueError("qp must be within 1..51")


def _to_blocks(plane: np.ndarray) -> np.ndarray:
    h, w = plane.shape
    return plane.reshape(h // BLOCK, BLOCK, w // BLOCK, BLOCK).transpose(0, 2, 1, 3).reshape(-1, BLOCK, BLOCK)


def _from_blocks(blocks: np.ndarray, h: int, w: int) -> np.ndarray:
    return blocks.reshape(h // BLOCK, w // BLOCK, BLOCK, BLOCK).transpose(0, 2, 1, 3).reshape(h, w)


def _rle_pack(levels: np.ndarray) -> bytes:
    """Run-length pack zigzag-ordered int16 levels: zero runs as (0, run).

    Vectorized via run-boundary detection; only zero-run segments are walked
    explicitly, which keeps large sparse planes in NumPy land.
    """
    flat = levels.reshape(-1).astype(np.int16)
    is_zero = flat == 0
    diff = np.diff(is_zero.astype(np.int8), prepend=0, append=0)
    starts = np.flatnonzero(diff == 1)
    ends = np.flatnonzero(diff == -1)
    pieces: list[np.ndarray] = []
    cursor = 0
    for start, end in zip(starts, ends):
        if start > cursor:
            pieces.append(flat[cursor:start])
        run = int(end - start)
        while run:
            step = min(run, 32767)
            pieces.append(np.array([0, step], dtype=np.int16))
            run -= step
        cursor = end
    if cursor < flat.size:
        pieces.append(flat[cursor:])
    if not pieces:
        return np.empty(0, dtype=np.int16).tobytes()
    return np.concatenate(pieces).tobytes()


def _rle_unpack(data: bytes, expected: int) -> np.ndarray:
    tokens = np.frombuffer(data, dtype=np.int16)
    out = np.empty(expected, dtype=np.int16)
    pos = 0
    i = 0
    while i < tokens.size:
        value = int(tokens[i])
        if value != 0:
            out[pos] = value
            pos += 1
            i += 1
        else:
            run = int(tokens[i + 1])
            out[pos:pos + run] = 0
            pos += run
            i += 2
    if pos != expected:
        raise ValueError(f"RLE decoded {pos} levels, expected {expected}")
    return out


def _encode_planes(planes, qp: int, chroma_qp: int) -> bytes:
    chunks: list[bytes] = []
    for idx, plane in enumerate(planes):
        table = quant_table(chroma_qp if idx else qp, chroma=bool(idx))
        blocks = _to_blocks(plane.astype(np.int64)) - 128
        levels = quantize(dct_blocks(blocks), table)
        packed = _rle_pack(zigzag_scan(levels.astype(np.int16)))
        chunks.append(zlib.compress(packed, 6))
    return b"".join(struct.pack(">I", len(c)) + c for c in chunks)


def _decode_planes(blob: bytes, h: int, w: int, qp: int, chroma_qp: int):
    offsets = [(h, w), (h // 2, w // 2), (h // 2, w // 2)]
    pos = 0
    planes = []
    for idx, (ph, pw) in enumerate(offsets):
        (size,) = struct.unpack(">I", blob[pos:pos + 4])
        pos += 4
        packed = zlib.decompress(blob[pos:pos + size])
        pos += size
        table = quant_table(chroma_qp if idx else qp, chroma=bool(idx))
        count = (ph // BLOCK) * (pw // BLOCK) * BLOCK * BLOCK
        seq = _rle_unpack(packed, count)
        blocks = zigzag_unscan(seq.reshape(-1, BLOCK * BLOCK)).astype(np.int64)
        recon = idct_blocks(dequantize(blocks, table)) + 128
        planes.append(np.clip(_from_blocks(recon, ph, pw), 0, 255).astype(np.uint8))
    return planes


def reconstruct_frame(payload: bytes, height: int, width: int, qp: int,
                       chroma_qp: int, previous: Frame | None) -> Frame:
    """Decode one frame payload, applying P-frame inter prediction from a ref."""
    planes = _decode_planes(payload, height, width, qp, chroma_qp)
    if previous is None:
        return Frame(*planes)
    rebuilt = tuple(
        np.clip(plane.astype(np.int16) + prev.astype(np.int16) - 128, 0, 255).astype(np.uint8)
        for plane, prev in zip(planes, (previous.y, previous.u, previous.v))
    )
    return Frame(*rebuilt)


def encode(frames, params: EncodeParams, out_path: str | Path) -> int:
    """Encode frames, write via temp file + atomic rename; return byte size."""
    params.validate()
    chroma_qp = min(51, params.qp + 2)
    target = Path(out_path)
    target.parent.mkdir(parents=True, exist_ok=True)
    tmp = target.with_name(target.name + ".tmp")
    n = len(frames)
    previous_recon: Frame | None = None
    with open(tmp, "wb") as fh:
        fh.write(HEADER.pack(MAGIC, params.width, params.height, n, params.fps, params.gop))
        for index, src in enumerate(frames):
            frame = src if (src.width, src.height) == (params.width, params.height) else resize(src, params.width, params.height)
            is_intra = index % params.gop == 0
            if is_intra or previous_recon is None:
                payload = _encode_planes((frame.y, frame.u, frame.v), params.qp, chroma_qp)
                ftype = 0
            else:
                residuals = tuple(
                    np.clip(cur.astype(np.int16) - ref.astype(np.int16) + 128, 0, 255).astype(np.uint8)
                    for cur, ref in zip((frame.y, frame.u, frame.v),
                                        (previous_recon.y, previous_recon.u, previous_recon.v))
                )
                payload = _encode_planes(residuals, params.qp, chroma_qp)
                ftype = 1
            fh.write(FRAME_HDR.pack(ftype, len(payload), zlib.crc32(payload) & 0xFFFFFFFF))
            fh.write(payload)
            previous_recon = reconstruct_frame(
                payload, params.height, params.width, params.qp, chroma_qp,
                None if is_intra else previous_recon,
            )
        fh.flush()
        os.fsync(fh.fileno())
    os.replace(tmp, target)
    return target.stat().st_size


def decode(path: str | Path, expected_qp: int) -> list[Frame]:
    """Read a bitstream back and reconstruct frames; CRC guards each frame."""
    with open(path, "rb") as fh:
        magic, width, height, count, fps, gop = HEADER.unpack(fh.read(HEADER.size))
        if magic != MAGIC:
            raise ValueError(f"{path}: bad magic {magic!r}")
        chroma_qp = min(51, expected_qp + 2)
        decoded: list[Frame] = []
        previous: Frame | None = None
        for index in range(count):
            ftype, size, crc = FRAME_HDR.unpack(fh.read(FRAME_HDR.size))
            payload = fh.read(size)
            if len(payload) != size or (zlib.crc32(payload) & 0xFFFFFFFF) != crc:
                raise ValueError(f"{path}: frame {index} corrupt or truncated")
            frame = reconstruct_frame(
                payload, height, width, expected_qp, chroma_qp,
                None if ftype == 0 else previous,
            )
            decoded.append(frame)
            previous = frame
    return decoded
