"""Packfile v2 writer/parser with copy/insert delta encoding.

Objects are stored either whole (zlib) or as a delta against a base object
(also zlib). Applying deltas is strict: any truncated or out-of-range
instruction raises :class:`DeltaCorruption` rather than returning a partial
object.
"""
from __future__ import annotations

import hashlib
import os
import struct
import zlib
from typing import Sequence

__all__ = ["PackWriter", "PackIndex", "PackEntry", "DeltaCorruption", "PackFormatError",
           "make_delta", "apply_delta", "read_packfile", "OBJ_BLOB", "OBJ_TREE", "OBJ_COMMIT",
           "MAX_DELTA_CANDIDATES"]

# git pack object type numbers
OBJ_COMMIT, OBJ_TREE, OBJ_BLOB, OBJ_TAG = 1, 2, 3, 4
OBJ_OFS_DELTA = 6
OBJ_REF_DELTA = 7
_TYPE_NAMES = {OBJ_BLOB: b"blob", OBJ_TREE: b"tree", OBJ_COMMIT: b"commit"}
_NAME_TYPES = {v: k for k, v in _TYPE_NAMES.items()}

SIGNATURE = b"PACK"
VERSION = 2
MAX_DELTA_CANDIDATES = 10
_MIN_MATCH = 16
_COPY_WINDOW = 64



class DeltaCorruption(Exception):
    """A delta or object could not be decoded exactly."""


class PackFormatError(Exception):
    """The packfile structure itself is invalid."""


class PackEntry:
    __slots__ = ("offset", "type", "size", "size_in_pack", "base_offset", "data_offset", "oid")

    def __init__(self, offset: int, type: int, size: int, size_in_pack: int,
                 base_offset: int | None, data_offset: int, oid: str | None = None) -> None:
        self.offset = offset
        self.type = type
        self.size = size
        self.size_in_pack = size_in_pack
        self.base_offset = base_offset
        self.data_offset = data_offset
        self.oid = oid


# ---------------------------------------------------------------- delta codec
def _emit_size(out: bytearray, size: int) -> None:
    """Little-endian base-128 varint, high bit = "more bytes follow"."""
    while True:
        b = size & 0x7F
        size >>= 7
        if size:
            out.append(b | 0x80)
        else:
            out.append(b)
            return


def _read_size(data: bytes, pos: int) -> tuple[int, int]:
    size = 0
    shift = 0
    while True:
        if pos >= len(data):
            raise DeltaCorruption("truncated delta size header")
        b = data[pos]
        pos += 1
        size |= (b & 0x7F) << shift
        shift += 7
        if not b & 0x80:
            return size, pos


def _emit_copy(out: bytearray, off: int, length: int) -> None:
    """git-style copy instruction: 0x80 | offset bytes | size bytes."""
    while length > 0:
        sz = min(length, 0xFFFF)
        cmd = 0x80
        buf = bytearray()
        for i, shift in enumerate((0, 8, 16, 24)):
            if off & (0xFF << shift):
                cmd |= 1 << i
                buf.append((off >> shift) & 0xFF)
        for i, shift in enumerate((0, 8, 16)):
            if sz & (0xFF << shift):
                cmd |= 0x10 << i
                buf.append((sz >> shift) & 0xFF)
        if sz == 0x10000:
            sz = 0
        out.append(cmd)
        out += buf
        off += sz if sz else 0x10000
        length -= sz if sz else 0x10000


def _emit_insert(out: bytearray, payload: bytes) -> None:
    i = 0
    while i < len(payload):
        n = min(len(payload) - i, 0x7F)
        out.append(n)
        out += payload[i:i + n]
        i += n


def make_delta(base: bytes, target: bytes, window: int = _COPY_WINDOW) -> bytes:
    """Encode ``target`` as copy/insert instructions against ``base``.

    Only matches at least ``_MIN_MATCH`` bytes long are emitted as copies; the
    rest becomes literal inserts. The match search is restricted to a sliding
    window around the current position, which bounds memory and time.
    """
    out = bytearray()
    blen = len(base)
    tlen = len(target)
    # git deltas are self-describing: base size and result size lead the stream.
    _emit_size(out, blen)
    _emit_size(out, tlen)
    lit = bytearray()
    i = 0
    while i < tlen:
        best_len = 0
        best_off = 0
        # A copy needs at least _MIN_MATCH bytes available on both sides.
        if blen and tlen - i >= _MIN_MATCH:
            lo = max(0, i - window)
            hi = min(blen, i + window)
            p = base.find(target[i:i + _MIN_MATCH], lo, hi + _MIN_MATCH)
            if p >= 0 and p < hi:
                ln = _MIN_MATCH
                while (ln < 0xFFFF and p + ln < blen and i + ln < tlen
                       and base[p + ln] == target[i + ln]):
                    ln += 1
                best_len, best_off = ln, p
        if best_len >= _MIN_MATCH:
            if lit:
                _emit_insert(out, bytes(lit))
                lit = bytearray()
            _emit_copy(out, best_off, best_len)
            i += best_len
        else:
            lit.append(target[i])
            i += 1
    if lit:
        _emit_insert(out, bytes(lit))
    return bytes(out)


def apply_delta(base: bytes, delta: bytes) -> bytes:
    """Apply a delta to ``base``, returning the reconstructed object.

    Raises :class:`DeltaCorruption` on any malformed instruction, on a copy
    that runs past the end of the base, or on a truncated instruction stream.
    """
    if not delta:
        raise DeltaCorruption("empty delta stream")
    out = bytearray()
    pos = 0
    end = len(delta)
    base_size, pos = _read_size(delta, pos)
    if base_size != len(base):
        raise DeltaCorruption(
            f"delta expects a base of {base_size} bytes, got {len(base)}")
    result_size, pos = _read_size(delta, pos)
    blen = len(base)
    while pos < end:
        cmd = delta[pos]
        pos += 1
        if cmd & 0x80:
            off = 0
            size = 0
            for i in range(4):
                if cmd & (1 << i):
                    if pos >= end:
                        raise DeltaCorruption("truncated copy offset")
                    off |= delta[pos] << (i * 8)
                    pos += 1
            for i in range(3):
                if cmd & (0x10 << i):
                    if pos >= end:
                        raise DeltaCorruption("truncated copy size")
                    size |= delta[pos] << (i * 8)
                    pos += 1
            if size == 0:
                size = 0x10000
            if off + size > blen:
                raise DeltaCorruption(
                    f"copy [{off},{off + size}) exceeds base of {blen} bytes")
            out += base[off:off + size]
        elif cmd:
            if pos + cmd > end:
                raise DeltaCorruption("truncated insert literal")
            out += delta[pos:pos + cmd]
            pos += cmd
        else:
            raise DeltaCorruption("reserved opcode 0x00 in delta")
    if result_size and len(out) != result_size:
        raise DeltaCorruption(
            f"delta produced {len(out)} bytes, header declared {result_size}")
    return bytes(out)



# ---------------------------------------------------------------- pack index
def write_pack_index(pack_path: str, entries: Sequence[PackEntry], oids: Sequence[str]) -> str:
    """Write a pack v2 .idx next to ``pack_path`` and return its path.

    Layout: magic, version, 256-entry fanout, sorted (sha1, offset) table,
    trailing pack checksum and idx checksum.
    """
    pairs = sorted(zip(oids, (e.offset for e in entries)), key=lambda p: p[0])
    fanout = [0] * 256
    for oid, _ in pairs:
        fanout[int(oid[:2], 16)] += 1
    running = 0
    for i in range(256):
        running += fanout[i]
        fanout[i] = running
    body = bytearray(b"\xfftOc" + struct.pack(">I", 2))
    body += struct.pack(">256I", *fanout)
    for oid, _ in pairs:
        body += bytes.fromhex(oid)
    # CRC32 of each object's packed representation, one per entry, in the same
    # order. Consumers may skip this table, but it must be present for the
    # offset table that follows to line up.
    for _ in pairs:
        body += struct.pack(">I", 0)
    for _, off in pairs:
        body += struct.pack(">I", off)
    # The idx records the pack's own trailer checksum, not a fresh digest.
    with open(pack_path, "rb") as pf:
        pf.seek(0, os.SEEK_END)
        size = pf.tell()
        if size < 20:
            raise PackFormatError("packfile too short to carry a checksum")
        pf.seek(-20, os.SEEK_END)
        pack_checksum = pf.read(20)
    body += pack_checksum
    body += hashlib.sha1(bytes(body)).digest()
    # git convention: foo.pack -> foo.idx
    idx_path = pack_path[:-5] + ".idx" if pack_path.endswith(".pack") else pack_path + ".idx"
    with open(idx_path, "wb") as fh:
        fh.write(bytes(body))
    return idx_path


def read_pack_index(path: str) -> dict[str, int]:
    """Read a .idx into an {oid: offset} map."""
    with open(path, "rb") as fh:
        data = fh.read()
    if data[:4] != b"\xfftOc":
        raise PackFormatError("bad pack index signature")
    version = struct.unpack(">I", data[4:8])[0]
    if version != 2:
        raise PackFormatError(f"unsupported pack index version {version}")
    fan = struct.unpack(">256I", data[8:8 + 1024])
    n = fan[255]
    base = 8 + 1024
    oids = [data[base + 20 * i: base + 20 * i + 20].hex() for i in range(n)]
    obase = base + 20 * n + 4 * n          # skip the CRC32 table
    offsets = [struct.unpack(">I", data[obase + 4 * i: obase + 4 * i + 4])[0] for i in range(n)]
    return dict(zip(oids, offsets))


def _pack_header(obj_type: int, size: int) -> bytearray:
    """Encode a pack object header: 3 type bits in the high nibble, then size.

    The low nibble of the first byte holds the low 4 bits of the size. Each
    following byte contributes 7 more bits; the 0x80 bit on every byte except
    the last signals that another byte follows.
    """
    out = bytearray()
    b = (obj_type << 4) | (size & 0x0F)
    size >>= 4
    while size:
        out.append(b | 0x80)
        b = size & 0x7F
        size >>= 7
    out.append(b)
    return out


# ---------------------------------------------------------------- pack writer
class PackWriter:
    """Builds a packfile v2 in memory-free fashion, one object at a time."""

    def __init__(self) -> None:
        self._chunks: list[bytes] = []
        self._offset = 0
        self.entries: list[PackEntry] = []

    @property
    def size(self) -> int:
        return self._offset

    def _emit(self, data: bytes) -> None:
        self._chunks.append(data)
        self._offset += len(data)

    def add_full(self, obj_type: int, payload: bytes) -> PackEntry:
        offset = self._offset
        if not self.entries:
            self._emit(b"PACK" + struct.pack(">II", VERSION, 0))
            offset = 12
        header = bytes(_pack_header(obj_type, len(payload)))
        data_offset = offset + len(header)
        self._emit(header + zlib.compress(payload, 6))
        e = PackEntry(offset, obj_type, len(payload), len(payload), None, data_offset, None)
        self.entries.append(e)
        return e

    def add_delta(self, base_index: int, delta: bytes) -> PackEntry:
        """Append an object encoded as a delta against a previously packed one.

        The object's real type is inherited from its base, as in the pack
        format; deltas are stored as OFS_DELTA with a back-reference.
        """
        if not 0 <= base_index < len(self.entries):
            raise IndexError("delta base must be a previously packed object")
        base_entry = self.entries[base_index]
        offset = self._offset
        dist = offset - base_entry.offset
        offs = bytearray()
        offs.append(dist & 0x7F)
        dist >>= 7
        while dist:
            dist -= 1
            offs.append(0x80 | (dist & 0x7F))
            dist >>= 7
        header = bytes(_pack_header(OBJ_OFS_DELTA, len(delta))) + bytes(reversed(offs))
        # compressed payload begins after the type/size header and the offset varint
        data_offset = offset + len(header)
        self._emit(header + zlib.compress(delta, 6))
        e = PackEntry(offset, OBJ_OFS_DELTA, len(delta), 0, base_entry.offset, data_offset, None)
        self.entries.append(e)
        return e

    def finish(self) -> bytes:
        """Trailer: SHA1 of everything above it, then the object count.

        Per the pack format the count is *excluded* from the checksum, so the
        digest covers the header and object data only.
        """
        out = bytearray(b"".join(self._chunks))
        # backfill the object count in the header now that it is known
        struct.pack_into(">I", out, 8, len(self.entries))
        # Trailer is the SHA1 of every preceding byte (header + objects).
        out += hashlib.sha1(bytes(out)).digest()
        return bytes(out)

    def write(self, path: str) -> str:
        data = self.finish()
        tmp = path + ".tmp"
        with open(tmp, "wb") as fh:
            fh.write(data)
        os.replace(tmp, path)
        return path


# ---------------------------------------------------------------- pack reader
def _read_varint_header(data: bytes, pos: int) -> tuple[int, int, int]:
    """Read a packed object header. Returns (type, size, new_pos)."""
    if pos >= len(data):
        raise PackFormatError("truncated object header")
    b = data[pos]
    pos += 1
    obj_type = (b >> 4) & 0x07
    size = b & 0x0F
    shift = 4
    while b & 0x80:
        if pos >= len(data):
            raise PackFormatError("truncated object header (size)")
        b = data[pos]
        pos += 1
        size |= (b & 0x7F) << shift
        shift += 7
    return obj_type, size, pos


def _read_ofs_delta_offset(data: bytes, pos: int) -> tuple[int, int]:
    if pos >= len(data):
        raise PackFormatError("truncated ofs-delta header")
    b = data[pos]
    pos += 1
    off = b & 0x7F
    while b & 0x80:
        if pos >= len(data):
            raise PackFormatError("truncated ofs-delta offset")
        b = data[pos]
        pos += 1
        off = ((off + 1) << 7) | (b & 0x7F)
    return off, pos


def read_packfile(path: str) -> tuple[list[PackEntry], dict[str, int]]:
    """Parse a packfile into entries plus an offset->index map."""
    with open(path, "rb") as fh:
        data = fh.read()
    if len(data) < 32:
        raise PackFormatError("packfile too short")
    if data[:4] != SIGNATURE:
        raise PackFormatError("bad pack signature")
    version, count = struct.unpack(">II", data[4:12])
    if version != VERSION:
        raise PackFormatError(f"unsupported pack version {version}")
    body_end = len(data) - 20          # everything before the SHA1 trailer
    if hashlib.sha1(data[:body_end]).digest() != data[body_end:]:
        raise PackFormatError("packfile checksum mismatch")
    entries: list[PackEntry] = []
    by_offset: dict[str, int] = {}
    pos = 12
    for _ in range(count):
        offset = pos
        obj_type, size, pos = _read_varint_header(data, pos)
        base_offset = None
        if obj_type == OBJ_OFS_DELTA:
            dist, pos = _read_ofs_delta_offset(data, pos)
            base_offset = offset - dist
        elif obj_type == OBJ_REF_DELTA:
            if pos + 20 > len(data):
                raise PackFormatError("truncated ref-delta")
            pos += 20
        comp_start = pos
        dec = zlib.decompressobj()
        out = dec.decompress(data[comp_start:body_end])
        if not dec.eof:
            raise DeltaCorruption(f"object at {offset} has truncated zlib stream")
        pos = comp_start + (len(data[comp_start:body_end]) - len(dec.unused_data))
        entries.append(PackEntry(offset, obj_type, size, len(out), base_offset, comp_start))
        by_offset[offset] = len(entries) - 1
    return entries, by_offset


def unpack_object(entries: list[PackEntry], idx: int, data: bytes,
                   offset_map: dict[int, int] | None = None,
                   cache: dict[int, tuple[int, bytes]] | None = None,
                   depth: int = 0) -> tuple[int, bytes]:
    """Resolve an object (following deltas) to (type, payload)."""
    if depth > 50:
        raise DeltaCorruption("delta chain too deep")
    cache = cache if cache is not None else {}
    if idx in cache:
        return cache[idx]
    e = entries[idx]
    if e.type in (OBJ_BLOB, OBJ_TREE, OBJ_COMMIT):
        payload = _inflate_at(data, e)
        cache[idx] = (e.type, payload)
        return cache[idx]
    if e.type == OBJ_OFS_DELTA:
        if e.base_offset is None:
            raise DeltaCorruption("ofs-delta without base offset")
        bidx = offset_map.get(e.base_offset) if offset_map else None
        if bidx is None:
            raise DeltaCorruption(f"ofs-delta base offset {e.base_offset} not found")
        base_type, base_payload = unpack_object(entries, bidx, data, offset_map, cache, depth + 1)
        delta = _inflate_at(data, e)
        result = (base_type, apply_delta(base_payload, delta))
    elif e.type == OBJ_REF_DELTA:
        delta = _inflate_at(data, e)
        raise DeltaCorruption("ref-delta base lookup by oid is not supported here")
    else:
        raise PackFormatError(f"unknown object type {e.type}")
    cache[idx] = result
    return result


def _inflate_at(data: bytes, e: PackEntry) -> bytes:
    dec = zlib.decompressobj()
    out = dec.decompress(data[e.data_offset:])
    if not dec.eof:
        raise DeltaCorruption(f"object at {e.offset} has truncated zlib stream")
    if len(out) != e.size:
        raise DeltaCorruption(
            f"object at {e.offset}: expected {e.size} bytes, inflated {len(out)}")
    return out
