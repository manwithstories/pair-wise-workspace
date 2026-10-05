"""Content-defined chunking via Rabin-Karp fingerprints over GF(2)[x]/P.

Boundaries depend only on byte content, never on absolute offset, so an
insertion or deletion early in a blob leaves every later block identical.
"""
from __future__ import annotations

import io
from typing import BinaryIO, Iterator, NamedTuple

__all__ = ["Chunk", "chunker", "chunk_bytes", "chunk_stream", "MIN_SIZE", "MAX_SIZE", "WINDOW"]

_MASK64 = (1 << 64) - 1
_POLY = 0xBFE6B8A5BF378D83      # degree-63 irreducible over GF(2)
_DEG = 63
_TOP = 1 << _DEG

WINDOW = 48
MIN_SIZE = 512
MAX_SIZE = 8192
_MASK = 0x0F                   # 5 significant bits -> ~1 cut / 2048B
_SHIFT = 1                     # bit0 always mirrors the last window byte
_ANCHOR = 0x01


def _reduce(val: int, limit: int) -> int:
    for bit in range(val.bit_length() - 1, limit - 1, -1):
        if (val >> bit) & 1:
            val ^= _POLY << (bit - _DEG)
    return val


_POS = tuple(
    tuple(_reduce(b << (WINDOW - 1 - p), _DEG) for b in range(256))
    for p in range(WINDOW)
)


class Rabin:
    """Sliding fingerprint over a fixed-size byte window.

    ``slide`` updates the fingerprint in O(1) using the precomputed per-position
    tables, so the window can advance byte by byte without rehashing.
    """

    __slots__ = ("h", "n")

    def __init__(self) -> None:
        self.h = 0
        self.n = 0

    def seed(self, data: bytes | bytearray | memoryview, end: int) -> int:
        h = 0
        p = 0
        for i in range(end - WINDOW, end):
            h ^= _POS[p][data[i]]
            p += 1
        self.h = h
        self.n = WINDOW
        return h

    def slide(self, out_b: int, in_b: int) -> int:
        h = self.h ^ _POS[0][out_b]
        h = ((h << 1) & _MASK64) ^ (_POLY if h & _TOP else 0)
        self.h = h ^ _POS[WINDOW - 1][in_b]
        self.n += 1
        return self.h

    def value_at(self, data, end: int) -> int:
        """Fingerprint of the window ending at ``end`` (recomputed in O(W))."""
        h = 0
        p = 0
        for i in range(end - WINDOW, end):
            h ^= _POS[p][data[i]]
            p += 1
        return h


class Chunk(NamedTuple):
    offset: int
    length: int


def chunk_bytes(data: bytes, min_size: int = MIN_SIZE, max_size: int = MAX_SIZE,
                mask: int = _MASK) -> list[Chunk]:
    """Split ``data`` into content-defined chunks.

    Scan bounds come from the current block start, so every emitted block is
    within [min_size, max_size] by construction; only the final block of a
    stream may be shorter than min_size.
    """
    n = len(data)
    if n == 0:
        return []
    out: list[Chunk] = []
    start = 0
    fp = Rabin()
    find = data.find
    anchor = bytes((_ANCHOR,))
    while start < n:
        lo = start + min_size - 1
        limit = min(start + max_size, n)
        cut = -1
        j = find(anchor, lo)
        while j >= 0 and j + 1 <= limit:
            if ((fp.value_at(data, j + 1) >> _SHIFT) & mask) == 0:
                cut = j + 1
                break
            j = find(anchor, j + 1)
        if cut < 0:
            cut = limit
        out.append(Chunk(start, cut - start))
        start = cut
    return out


def chunk_stream(reader: BinaryIO, min_size: int = MIN_SIZE, max_size: int = MAX_SIZE,
                 mask: int = _MASK, read_size: int = 1 << 18) -> Iterator[tuple[int, bytes]]:
    """Stream chunks without holding the whole blob in memory.

    Yields ``(offset, block)``. At most ``max_size + read_size`` bytes are
    resident, so memory is bounded regardless of input size. Emits exactly the
    same block sequence as :func:`chunk_bytes`.

    ``read_size`` trades syscall count against resident memory; 256 KiB keeps
    peak usage negligible without measurably changing throughput.
    """
    buf = bytearray()
    base = 0
    cut = 0
    eof = False
    fp = Rabin()
    anchor = bytes((_ANCHOR,))
    while True:
        # Guarantee a full scan window, or accept the tail at EOF.
        while not eof and len(buf) < max_size:
            more = reader.read(read_size)
            if not more:
                eof = True
                break
            buf += more
        n = len(buf)
        if n == 0:
            return
        lo = cut - base + min_size - 1
        limit = min(cut - base + max_size, n)
        hit = -1
        if lo >= 0 and lo < n:
            j = buf.find(anchor, lo)
            while j >= 0 and j + 1 <= limit:
                if ((fp.value_at(buf, j + 1) >> _SHIFT) & mask) == 0:
                    hit = j + 1
                    break
                j = buf.find(anchor, j + 1)
        if hit < 0:
            # A decision needs a full max_size window; without one (short tail
            # at EOF) the remainder becomes the final block.
            if not eof and n - (cut - base) < max_size:
                continue
            hit = limit
        block = bytes(buf[:hit])
        del buf[:hit]
        base += hit
        yield cut, block
        cut += hit


class chunker:
    """Reusable chunker facade with a fixed parameter set."""

    def __init__(self, min_size: int = MIN_SIZE, max_size: int = MAX_SIZE, mask: int = _MASK) -> None:
        if min_size < 1 or max_size < min_size:
            raise ValueError("invalid chunk bounds")
        self.min_size = min_size
        self.max_size = max_size
        self.mask = mask

    def split(self, data: bytes) -> list[Chunk]:
        return chunk_bytes(data, self.min_size, self.max_size, self.mask)

    def split_stream(self, reader: BinaryIO) -> Iterator[tuple[int, bytes]]:
        return chunk_stream(reader, self.min_size, self.max_size, self.mask)

    def iter_chunks(self, data: bytes) -> Iterator[bytes]:
        for c in self.split(data):
            yield data[c.offset:c.offset + c.length]
