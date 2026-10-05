"""Content-addressed loose object store: blob / tree / commit.

Objects are stored git-style as ``"<type> <size>\\0<payload>"`` and named by the
SHA1 of that full byte string. Payload writing is streaming, so a multi-GB
sample file never has to be resident in memory all at once.
"""
from __future__ import annotations

import hashlib
import os
import threading
import zlib
from typing import BinaryIO, Iterator

__all__ = ["ObjectStore", "Blob", "TreeEntry", "Commit", "OBJ_BLOB", "OBJ_TREE",
           "OBJ_COMMIT", "MODE_FILE", "MODE_EXEC", "MODE_LINK", "MODE_TREE",
           "DeltaCorruption", "hash_object", "hash_blob", "build_tree_object"]

OBJ_BLOB = b"blob"
OBJ_TREE = b"tree"
OBJ_COMMIT = b"commit"

MODE_TREE = 0o040000
MODE_FILE = 0o100644
MODE_EXEC = 0o100755
MODE_LINK = 0o120000

_READ_SIZE = 1 << 20


class DeltaCorruption(Exception):
    """Raised when a stored object cannot be decoded exactly."""


def hash_object(obj_type: bytes, payload: bytes) -> str:
    h = hashlib.sha1()
    h.update(b"%s %d\0" % (obj_type, len(payload)))
    h.update(payload)
    return h.hexdigest()


def hash_blob(data: bytes) -> str:
    return hash_object(OBJ_BLOB, data)


def _encode_tree(entries: list[tuple[int, bytes, str]]) -> bytes:
    """Encode git tree entries. Entries must be sorted by name."""
    out = bytearray()
    for mode, name, oid in sorted(entries, key=lambda e: e[1] + (b"/" if e[0] == MODE_TREE else b"")):
        out += b"%o %s\0" % (mode, name)
        out += bytes.fromhex(oid)
    return bytes(out)


def build_tree_object(entries: list[tuple[int, bytes, str]]) -> bytes:
    return _encode_tree(entries)


class Blob:
    __slots__ = ("data", "oid")

    def __init__(self, data: bytes, oid: str | None = None) -> None:
        self.data = data
        self.oid = oid or hash_blob(data)

    def __len__(self) -> int:
        return len(self.data)


class TreeEntry:
    __slots__ = ("mode", "name", "oid")

    def __init__(self, mode: int, name: bytes, oid: str) -> None:
        self.mode = mode
        self.name = name
        self.oid = oid

    @property
    def is_dir(self) -> bool:
        return self.mode == MODE_TREE

    def __repr__(self) -> str:
        return f"TreeEntry({self.mode:o}, {self.name!r}, {self.oid[:8]})"


class Commit:
    __slots__ = ("tree", "parents", "author", "committer", "message", "raw")

    def __init__(self, tree: str, parents: list[str], author: str, committer: str,
                 message: str, raw: bytes | None = None) -> None:
        self.tree = tree
        self.parents = parents
        self.author = author
        self.committer = committer
        self.message = message
        self.raw = raw

    def serialize(self) -> bytes:
        out = [b"tree " + self.tree.encode()]
        for p in self.parents:
            out.append(b"parent " + p.encode())
        out.append(b"author " + self.author.encode())
        out.append(b"committer " + self.committer.encode())
        return b"\n".join(out) + b"\n\n" + self.message.encode()

    @property
    def oid(self) -> str:
        return hash_object(OBJ_COMMIT, self.raw or self.serialize())


def parse_tree(payload: bytes) -> list[TreeEntry]:
    entries: list[TreeEntry] = []
    i = 0
    n = len(payload)
    while i < n:
        sp = payload.index(b" ", i)
        mode = int(payload[i:sp], 8)
        nul = payload.index(b"\0", sp)
        name = payload[sp + 1:nul]
        oid = payload[nul + 1:nul + 21].hex()
        entries.append(TreeEntry(mode, name, oid))
        i = nul + 21
    return entries


def parse_commit(payload: bytes) -> Commit:
    head, _, message = payload.partition(b"\n\n")
    tree = ""
    parents: list[str] = []
    author = committer = ""
    for line in head.split(b"\n"):
        k, _, v = line.partition(b" ")
        if k == b"tree":
            tree = v.decode()
        elif k == b"parent":
            parents.append(v.decode())
        elif k == b"author":
            author = v.decode()
        elif k == b"committer":
            committer = v.decode()
    return Commit(tree, parents, author, committer, message.decode("utf-8", "replace"), raw=payload)


class ObjectStore:
    """Loose object database rooted at ``<repo>/.wellsample/objects``.

    Concurrent readers are safe without locking: every object is written to a
    temp file and atomically renamed, so a reader either sees the complete
    object or no object at all. Writes are serialized by a single lock.
    """

    def __init__(self, root: str | os.PathLike) -> None:
        self.root = os.fspath(root)
        self.objects_dir = os.path.join(self.root, "objects")
        self._write_lock = threading.Lock()
        os.makedirs(self.objects_dir, exist_ok=True)

    # ---------- paths ----------
    def path_for(self, oid: str) -> str:
        return os.path.join(self.objects_dir, oid[:2], oid[2:])

    def exists(self, oid: str) -> bool:
        return os.path.exists(self.path_for(oid))

    def __len__(self) -> int:
        return sum(1 for _ in self.iter_oids())

    def iter_oids(self) -> Iterator[str]:
        for sub in sorted(os.listdir(self.objects_dir)):
            d = os.path.join(self.objects_dir, sub)
            if len(sub) != 2 or not os.path.isdir(d):
                continue
            for rest in sorted(os.listdir(d)):
                yield sub + rest

    # ---------- write ----------
    def _write_full(self, obj_type: bytes, payload: bytes) -> str:
        header = b"%s %d\0" % (obj_type, len(payload))
        oid = hashlib.sha1(header + payload).hexdigest()
        final = self.path_for(oid)
        if os.path.exists(final):
            return oid
        with self._write_lock:
            if os.path.exists(final):
                return oid
            os.makedirs(os.path.dirname(final), exist_ok=True)
            tmp = final + ".tmp%d" % os.getpid()
            with open(tmp, "wb") as fh:
                fh.write(zlib.compress(header + payload, 6))
            os.replace(tmp, final)
        return oid

    def write_blob(self, data: bytes) -> str:
        return self._write_full(OBJ_BLOB, data)

    def write_blob_stream(self, reader: BinaryIO, chunker=None) -> str:
        """Stream a blob to the store, hashing and compressing incrementally.

        The object header ("blob <size>\\0") precedes the payload, so the file is
        staged in a temp file and the header is written in a second pass once
        the total size is known. Peak memory is a few zlib buffers regardless of
        blob size -- the payload is never fully resident.
        """
        h = hashlib.sha1()
        total = 0
        os.makedirs(self.objects_dir, exist_ok=True)
        tmp = os.path.join(self.objects_dir, "tmp_stream_%d" % os.getpid())
        try:
            with open(tmp, "wb") as fh:
                while True:
                    block = reader.read(_READ_SIZE)
                    if not block:
                        break
                    total += len(block)
                    h.update(block)
                    fh.write(block)
            # size is now known; build the final compressed object
            header = b"blob %d\0" % total
            oid = hashlib.sha1(header).hexdigest()
            hh = hashlib.sha1(header)
            hh.update(b"")  # payload hashed in the pass below, streaming
            comp = zlib.compressobj(6)
            tmp2 = tmp + ".z"
            digest = hashlib.sha1(header)
            with open(tmp, "rb") as src_fh, open(tmp2, "wb") as dst:
                dst.write(comp.compress(header))
                running = hashlib.sha1(header)
                while True:
                    block = src_fh.read(_READ_SIZE)
                    if not block:
                        break
                    running.update(block)
                    dst.write(comp.compress(block))
                dst.write(comp.flush())
                oid = running.hexdigest()
            final = self.path_for(oid)
            os.makedirs(os.path.dirname(final), exist_ok=True)
            with self._write_lock:
                if os.path.exists(final):
                    os.unlink(tmp)
                    os.unlink(tmp2)
                    return oid
                os.replace(tmp2, final)
        finally:
            for p in (tmp, tmp + ".z"):
                if os.path.exists(p):
                    os.unlink(p)
        return oid

    def write_tree(self, entries: list[tuple[int, bytes, str]]) -> str:
        return self._write_full(OBJ_TREE, _encode_tree(entries))

    def write_commit(self, commit: Commit) -> str:
        return self._write_full(OBJ_COMMIT, commit.serialize())

    # ---------- read ----------
    def read_raw(self, oid: str) -> tuple[bytes, bytes]:
        """Return ``(type, payload)`` for a loose object."""
        path = self.path_for(oid)
        try:
            with open(path, "rb") as fh:
                raw = zlib.decompress(fh.read())
        except FileNotFoundError:
            raise KeyError(f"object {oid} not found") from None
        except zlib.error as exc:
            raise DeltaCorruption(f"object {oid} is not valid zlib: {exc}") from None
        nul = raw.index(b"\0")
        type_part, _, size_part = raw[:nul].partition(b" ")
        declared = int(size_part)
        payload = raw[nul + 1:]
        if len(payload) != declared:
            raise DeltaCorruption(
                f"object {oid} size mismatch: header says {declared}, got {len(payload)}")
        if hashlib.sha1(raw).hexdigest() != oid:
            raise DeltaCorruption(f"object {oid} failed SHA1 verification")
        return type_part, payload

    def read_blob(self, oid: str) -> bytes:
        t, payload = self.read_raw(oid)
        if t != OBJ_BLOB:
            raise DeltaCorruption(f"object {oid} is a {t.decode()}, expected blob")
        return payload

    def read_blob_stream(self, oid: str, read_size: int = _READ_SIZE) -> Iterator[bytes]:
        """Yield a blob's payload bytes without materializing it in memory.

        The ``"blob <size>\\0"`` header is consumed and discarded as the stream
        is decompressed, so callers see payload bytes only.
        """
        path = self.path_for(oid)
        try:
            fh = open(path, "rb")
        except FileNotFoundError:
            raise KeyError(f"object {oid} not found") from None
        with fh:
            dec = zlib.decompressobj()
            pending = b""
            started = False
            while True:
                raw = fh.read(read_size)
                if not raw:
                    break
                out = dec.decompress(raw)
                if started:
                    if out:
                        yield out
                elif out:
                    nul = out.find(b"\0")
                    if nul < 0:
                        pending += out
                        continue
                    started = True
                    rest = out[nul + 1:]
                    if rest:
                        yield rest
            for out in (dec.flush(),):
                if not started:
                    nul = out.find(b"\0")
                    if nul >= 0:
                        started = True
                        out = out[nul + 1:]
                if out:
                    yield out
            if not started:
                raise DeltaCorruption(f"object {oid} is missing its header")

    def read_tree(self, oid: str) -> list[TreeEntry]:
        t, payload = self.read_raw(oid)
        if t != OBJ_TREE:
            raise DeltaCorruption(f"object {oid} is a {t.decode()}, expected tree")
        return parse_tree(payload)

    def read_commit(self, oid: str) -> Commit:
        t, payload = self.read_raw(oid)
        if t != OBJ_COMMIT:
            raise DeltaCorruption(f"object {oid} is a {t.decode()}, expected commit")
        return parse_commit(payload)

    def read_object(self, oid: str) -> tuple[bytes, bytes]:
        return self.read_raw(oid)
