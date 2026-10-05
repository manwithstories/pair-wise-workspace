"""Working-tree index and byte-exact checkout.

Restores files to exactly the bytes recorded at commit time, preserving the
executable bit and symlink targets, and can verify a whole tree by hash.
"""
from __future__ import annotations

import hashlib
import os
import stat
from object_store import (MODE_EXEC, MODE_FILE, MODE_LINK, MODE_TREE, ObjectStore)

__all__ = ["Workspace", "IndexEntry", "CheckoutError"]


class CheckoutError(Exception):
    pass


class IndexEntry:
    __slots__ = ("path", "mode", "oid", "size")

    def __init__(self, path: str, mode: int, oid: str, size: int) -> None:
        self.path = path
        self.mode = mode
        self.oid = oid
        self.size = size

    def __repr__(self) -> str:
        return f"IndexEntry({self.path!r}, {self.mode:o}, {self.oid[:8]})"


class Workspace:
    def __init__(self, root: str | os.PathLike, store: ObjectStore) -> None:
        self.root = os.fspath(root)
        self.store = store

    # ---------- scanning ----------
    def _abs(self, rel: str) -> str:
        return os.path.join(self.root, rel)

    def scan(self) -> dict[str, IndexEntry]:
        """Walk the working tree, hashing every file into content-addressed blobs."""
        index: dict[str, IndexEntry] = {}
        root = os.path.abspath(self.root)
        for dirpath, dirnames, filenames in os.walk(root, followlinks=False):
            dirnames[:] = [d for d in dirnames if d != ".wellsample"]
            for name in filenames:
                full = os.path.join(dirpath, name)
                rel = os.path.relpath(full, root)
                if rel.startswith(".wellsample"):
                    continue
                lst = os.lstat(full)
                if stat.S_ISLNK(lst.st_mode):
                    target = os.readlink(full).encode()
                    oid = self.store.write_blob(target)
                    index[rel] = IndexEntry(rel, MODE_LINK, oid, len(target))
                elif stat.S_ISREG(lst.st_mode):
                    with open(full, "rb") as fh:
                        oid = self.store.write_blob_stream(fh)
                    size = os.path.getsize(full)
                    mode = MODE_EXEC if lst.st_mode & stat.S_IXUSR else MODE_FILE
                    index[rel] = IndexEntry(rel, mode, oid, size)
                # other types (fifo, socket) are not archived
        return index

    def write_tree_from_index(self, index: dict[str, IndexEntry]) -> str:
        """Build tree objects from a flat path->entry mapping."""
        root: dict = {}
        for rel, ent in index.items():
            parts = rel.split(os.sep)
            node = root
            for part in parts[:-1]:
                node = node.setdefault(part, {})
                if not isinstance(node, dict):
                    raise CheckoutError(f"path conflict at {rel}")
            node[parts[-1]] = ent
        return self._build(root)

    def _build(self, node: dict) -> str:
        entries = []
        for name, value in node.items():
            raw = name.encode("utf-8", "surrogateescape")
            if isinstance(value, dict):
                entries.append((MODE_TREE, raw, self._build(value)))
            else:
                entries.append((value.mode, raw, value.oid))
        return self.store.write_tree(entries)

    # ---------- checkout ----------
    def checkout(self, tree_oid: str | None, clean: bool = False) -> int:
        """Materialize a tree, byte for byte. Returns the file count written."""
        entries = self.flatten(tree_oid)
        if clean:
            self._remove_extraneous(entries)
        # Create each directory once instead of per file: for a 10k-file tree
        # this is the difference between one makedirs per file and one per dir.
        made: set[str] = set()
        for rel in entries:
            parent = os.path.dirname(rel)
            while parent and parent not in made:
                os.makedirs(self._abs(parent), exist_ok=True)
                made.add(parent)
                parent = os.path.dirname(parent)
        written = 0
        for rel, (mode, oid) in sorted(entries.items()):
            self._write_one(rel, mode, oid)
            written += 1
        return written

    def _write_one(self, rel: str, mode: int, oid: str) -> None:
        full = self._abs(rel)
        if mode == MODE_LINK:
            target = self.store.read_blob(oid)
            if os.path.islink(full) or os.path.exists(full):
                os.unlink(full)
            os.symlink(target.decode("utf-8", "surrogateescape"), full)
            return
        if os.path.islink(full):
            os.unlink(full)
        # os.open applies the mode in the same syscall that creates the file,
        # which is one call cheaper than open()+chmod() for every file.
        perm = 0o755 if mode == MODE_EXEC else 0o644
        fd = os.open(full, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, perm)
        try:
            with os.fdopen(fd, "wb") as fh:
                for block in self.store.read_blob_stream(oid):
                    fh.write(block)
        except BaseException:
            os.close(fd)
            raise
        # os.open honours umask, so restate the mode explicitly.
        os.chmod(full, perm)

    def _remove_extraneous(self, keep: dict[str, tuple[int, str]]) -> None:
        root = os.path.abspath(self.root)
        for dirpath, dirnames, filenames in os.walk(root, topdown=False, followlinks=False):
            dirnames[:] = [d for d in dirnames if d != ".wellsample"]
            for name in filenames + [d for d in dirnames
                                     if os.path.islink(os.path.join(dirpath, d))]:
                full = os.path.join(dirpath, name)
                rel = os.path.relpath(full, root)
                if rel not in keep:
                    os.unlink(full)
            if dirpath != root:
                try:
                    os.rmdir(dirpath)
                except OSError:
                    pass

    def flatten(self, tree_oid: str | None, prefix: str = "",
                out: dict[str, tuple[int, str]] | None = None) -> dict[str, tuple[int, str]]:
        """Expand a tree into path -> (mode, oid) for every non-tree entry."""
        if out is None:
            out = {}
        if not tree_oid:
            return out
        for e in self.store.read_tree(tree_oid):
            path = f"{prefix}{e.name.decode('utf-8', 'surrogateescape')}"
            if e.is_dir:
                self.flatten(e.oid, path + "/", out)
            else:
                out[path] = (e.mode, e.oid)
        return out

    # ---------- verification ----------
    def verify(self, tree_oid: str | None) -> list[str]:
        """Return a list of paths whose working-tree bytes differ from the tree."""
        problems: list[str] = []
        for rel, (mode, oid) in sorted(self.flatten(tree_oid).items()):
            full = self._abs(rel)
            if mode == MODE_LINK:
                if not os.path.islink(full):
                    problems.append(rel)
                    continue
                if os.readlink(full).encode() != self.store.read_blob(oid):
                    problems.append(rel)
                continue
            if os.path.islink(full) or not os.path.isfile(full):
                problems.append(rel)
                continue
            want = self.store.read_blob(oid)
            got = open(full, "rb").read()
            if got != want:
                problems.append(rel)
                continue
            want_exec = bool(mode == MODE_EXEC)
            has_exec = bool(os.stat(full).st_mode & stat.S_IXUSR)
            if want_exec != has_exec:
                problems.append(rel)
        return problems

    def tree_hash(self, tree_oid: str | None) -> str:
        """A stable digest over every path, mode and blob in the tree."""
        h = hashlib.sha1()
        for rel, (mode, oid) in sorted(self.flatten(tree_oid).items()):
            h.update(f"{mode:o} {rel}\0{oid}\0".encode("utf-8", "surrogateescape"))
        return h.hexdigest()
