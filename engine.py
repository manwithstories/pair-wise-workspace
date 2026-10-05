"""Terminal entry point: init, add/commit, checkout, merge, gc, verify.

    python -m engine demo --verify-byte-exact
"""
from __future__ import annotations

import argparse
import hashlib
import os
import random
import sys
import time
import zlib
from typing import Sequence

import chunker
from merge import render_conflict, three_way_merge
from object_store import Commit, ObjectStore
from packfile import (MAX_DELTA_CANDIDATES, OBJ_BLOB, PackWriter, make_delta,
                      write_pack_index)
from workspace import Workspace

__all__ = ["Repository", "main"]

REPO_DIR = ".wellsample"
DEFAULT_IDENTITY = "geo-archivist <archivist@wellsample.local>"


class Repository:
    """A repository: an object store, a working tree, and a HEAD commit."""

    def __init__(self, path: str | os.PathLike) -> None:
        self.path = os.path.abspath(os.fspath(path))
        self.gitdir = os.path.join(self.path, REPO_DIR)
        self.store = ObjectStore(self.gitdir)
        self.ws = Workspace(self.path, self.store)
        self._head_path = os.path.join(self.gitdir, "HEAD")
        self._index_path = os.path.join(self.gitdir, "index")

    # ---------- init ----------
    def init(self) -> None:
        os.makedirs(self.gitdir, exist_ok=True)
        os.makedirs(os.path.join(self.gitdir, "objects"), exist_ok=True)
        os.makedirs(os.path.join(self.gitdir, "packs"), exist_ok=True)
        if not os.path.exists(self._head_path):
            self._write_head("")
        print(f"initialized empty repository at {self.path}")

    def exists(self) -> bool:
        return os.path.isdir(self.gitdir)

    # ---------- head ----------
    def _write_head(self, oid: str) -> None:
        tmp = self._head_path + ".tmp"
        with open(tmp, "w") as fh:
            fh.write(oid + "\n")
        os.replace(tmp, self._head_path)

    def head(self) -> str | None:
        if not os.path.exists(self._head_path):
            return None
        oid = open(self._head_path).read().strip()
        return oid or None

    def set_head(self, oid: str) -> None:
        self._write_head(oid)

    # ---------- commit ----------
    def commit(self, message: str, identity: str = DEFAULT_IDENTITY) -> str:
        index = self.ws.scan()
        tree = self.ws.write_tree_from_index(index)
        parents = [h] if (h := self.head()) else []
        stamp = f"{identity} {int(time.time())} +0000"
        c = Commit(tree, parents, stamp, stamp, message)
        oid = self.store.write_commit(c)
        self._write_head(oid)
        print(f"[{oid[:8]}] {message} ({len(index)} files)")
        return oid

    def commit_tree(self, message: str, identity: str = DEFAULT_IDENTITY) -> str:
        """Commit the current working tree without rescanning."""
        index = self.ws.scan()
        tree = self.ws.write_tree_from_index(index)
        parents = [h] if (h := self.head()) else []
        stamp = f"{identity} {int(time.time())} +0000"
        c = Commit(tree, parents, stamp, stamp, message)
        oid = self.store.write_commit(c)
        self._write_head(oid)
        return oid

    def tree_of(self, commit_oid: str) -> str:
        return self.store.read_commit(commit_oid).tree

    def checkout(self, commit_oid: str | None = None, clean: bool = False) -> int:
        oid = commit_oid or self.head()
        if oid is None:
            raise SystemExit("no commits yet")
        tree = self.tree_of(oid)
        n = self.ws.checkout(tree, clean=clean)
        self._write_head(oid)
        return n

    def verify(self, commit_oid: str | None = None) -> list[str]:
        oid = commit_oid or self.head()
        if oid is None:
            return []
        return self.ws.verify(self.tree_of(oid))

    # ---------- merge ----------
    def merge(self, base: str, ours: str, theirs: str,
              conflict_dir: str | None = None) -> int:
        base_tree = self.tree_of(base) if base else None
        ours_tree = self.tree_of(ours)
        theirs_tree = self.tree_of(theirs)
        result = three_way_merge(self.store, base_tree, ours_tree, theirs_tree)
        if conflict_dir:
            os.makedirs(conflict_dir, exist_ok=True)
            for c in result.conflicts:
                ob = self.store.read_blob(self.ws.flatten(ours_tree)[c.ours][1]) if c.ours and c.ours in self.ws.flatten(ours_tree) else None
                tb = self.store.read_blob(self.ws.flatten(theirs_tree)[c.theirs][1]) if c.theirs and c.theirs in self.ws.flatten(theirs_tree) else None
                bb = None
                if base_tree and c.base and c.base in self.ws.flatten(base_tree):
                    bb = self.store.read_blob(self.ws.flatten(base_tree)[c.base][1])
                out = os.path.join(conflict_dir, c.path.replace("/", "__") + ".conflict")
                with open(out, "wb") as fh:
                    fh.write(render_conflict(c.path, ob, bb, tb))
        return len(result.conflicts)

    # ---------- gc / pack ----------
    def gc(self, delta_window: int = MAX_DELTA_CANDIDATES) -> dict:
        """Pack every loose object, delta-encoding similar blobs."""
        oids = list(self.store.iter_oids())
        loose_bytes = 0
        for oid in oids:
            loose_bytes += os.path.getsize(self.store.path_for(oid))

        # group blobs by type so trees/commits never become delta bases for blobs
        blobs: list[tuple[str, bytes]] = []
        others: list[tuple[int, bytes]] = []
        for oid in oids:
            t, payload = self.store.read_raw(oid)
            if t == b"blob":
                blobs.append((oid, payload))
            else:
                others.append((t, payload))

        w = PackWriter()
        oids_out: list[str] = []
        entries_oids: list[str] = []
        # similar-size blobs make the best delta bases
        blobs.sort(key=lambda kv: len(kv[1]))
        placed: list[tuple[str, int, bytes]] = []   # (oid, index, payload)

        for oid, payload in blobs:
            best_delta = None
            best_base = None
            for base_oid, bidx, bpayload in placed[-delta_window:]:
                d = make_delta(bpayload, payload)
                if best_delta is None or len(d) < len(best_delta):
                    best_delta, best_base = d, bidx
            # Only keep a delta when it is smaller than the object itself;
            # otherwise the object is cheaper to store whole. The comparison
            # must be against the compressed form, since that is what a full
            # entry costs in the pack.
            if best_delta is not None and len(zlib.compress(best_delta, 6)) < len(
                    zlib.compress(payload, 6)):
                bidx = best_base
                w.add_delta(bidx, best_delta)
                entries_oids.append(oid)
            else:
                w.add_full(OBJ_BLOB, payload)
                entries_oids.append(oid)
            placed.append((oid, len(w.entries) - 1, payload))

        for t, payload in others:
            tn = {b"tree": 2, b"commit": 1}[t]
            w.add_full(tn, payload)
            entries_oids.append(hashlib.sha1(t + b" %d\0" % len(payload) + payload).hexdigest())

        packs_dir = os.path.join(self.gitdir, "packs")
        os.makedirs(packs_dir, exist_ok=True)
        name = "pack-%s" % hashlib.sha1(w.finish()).hexdigest()[:16]
        base = os.path.join(packs_dir, name)
        with open(base + ".pack", "wb") as fh:
            fh.write(w.finish())
        write_pack_index(base + ".pack", w.entries, entries_oids)

        pack_bytes = os.path.getsize(base + ".pack") + os.path.getsize(base + ".idx")
        return {
            "objects": len(oids),
            "loose_bytes": loose_bytes,
            "pack_bytes": pack_bytes,
            "ratio": (loose_bytes / pack_bytes) if pack_bytes else 0.0,
            "pack": base + ".pack",
        }


# ---------------------------------------------------------------- demo
def _sample_blob(n: int, seed: int) -> bytes:
    """Pseudo-geological payload: repeated local structure, not pure noise."""
    r = random.Random(seed)
    pool = [bytes(r.getrandbits(8) for _ in range(r.randrange(200, 3000))) for _ in range(64)]
    out = bytearray()
    while len(out) < n:
        p = pool[r.randrange(64)]
        out += p[r.randrange(0, len(p)):]
    return bytes(out[:n])


def _build_demo_repo(root: str) -> Repository:
    """Exercise every feature: chunking, dedup, pack, merge, checkout."""
    repo = Repository(root)
    repo.init()
    ws = repo.ws

    survey = os.path.join(repo.path, "survey")
    os.makedirs(survey, exist_ok=True)

    # --- version 1
    payloads = {}
    for i in range(6):
        name = f"well_{i:02d}.segy"
        data = _sample_blob(48_000 + i * 1000, seed=i)
        payloads[name] = data
        with open(os.path.join(survey, name), "wb") as fh:
            fh.write(data)
    with open(os.path.join(survey, "run.sh"), "wb") as fh:
        fh.write(b"#!/bin/sh\necho survey\n")
    os.chmod(os.path.join(survey, "run.sh"), 0o755)
    os.symlink("well_00.segy", os.path.join(survey, "latest.segy"))
    with open(os.path.join(survey, "empty.dat"), "wb") as fh:
        fh.write(b"")
    with open(os.path.join(survey, "notes.txt"), "wb") as fh:
        fh.write(b"trailing newline\n")
    with open(os.path.join(survey, "no_eol.txt"), "wb") as fh:
        fh.write(b"no trailing newline")
    c1 = repo.commit("survey v1: baseline acquisition")

    # --- chunk/dedup demonstration
    v1 = payloads["well_00.segy"]
    v2 = v1[:4096] + b"INTERCALATED_TRACE" * 40 + v1[4096:]
    blocks_v1 = chunker.chunk_bytes(v1)
    blocks_v2 = chunker.chunk_bytes(v2)
    h1 = {hashlib.sha1(v1[o:o + l]).digest() for o, l in blocks_v1}
    shared = sum(1 for o, l in blocks_v2 if hashlib.sha1(v2[o:o + l]).digest() in h1)
    print(f"chunking: v1 -> {len(blocks_v1)} blocks, avg "
          f"{sum(l for _, l in blocks_v1) / len(blocks_v1):.0f}B; "
          f"after a mid-file edit {shared}/{len(blocks_v2)} blocks still dedup")

    # --- version 2: modify one file, add another
    payloads["well_00.segy"] = v2
    with open(os.path.join(survey, "well_00.segy"), "wb") as fh:
        fh.write(v2)
    with open(os.path.join(survey, "well_06.segy"), "wb") as fh:
        fh.write(_sample_blob(50_000, seed=99))
    c2 = repo.commit("survey v2: reprocessed trace, new well")

    # --- branch A: rename + edit, and a different take on the same new path
    os.rename(os.path.join(survey, "well_01.segy"), os.path.join(survey, "well_01b.segy"))
    with open(os.path.join(survey, "well_02.segy"), "ab") as fh:
        fh.write(b"appended by branch A\n")
    with open(os.path.join(survey, "interpretation.md"), "wb") as fh:
        fh.write(b"branch A interpretation of the survey\n")
    cA = repo.commit("branch A: rename well_01, append to well_02, own interpretation")

    # --- branch B: divergent add on the same path, plus an independent delete
    with open(os.path.join(survey, "well_03.segy"), "wb") as fh:
        fh.write(b"branch B replaced this file\n")
    with open(os.path.join(survey, "interpretation.md"), "wb") as fh:
        fh.write(b"branch B interpretation of the survey\n")
    os.makedirs(os.path.join(survey, "derived"), exist_ok=True)
    with open(os.path.join(survey, "derived", "summary.txt"), "wb") as fh:
        fh.write(b"derived output\n")
    cB = repo.commit("branch B: divergent interpretation, derived output")

    # --- three-way merge: base=c2, ours=cA, theirs=cB
    conflicts = repo.merge(c2, cA, cB,
                           conflict_dir=os.path.join(repo.path, "conflicts"))
    written = sorted(os.listdir(os.path.join(repo.path, "conflicts")))
    print(f"merge: {conflicts} conflict(s) -> {', '.join(written) if written else 'none'}")

    # --- pack (gc)
    stats = repo.gc()
    print(f"gc: {stats['objects']} objects, loose {stats['loose_bytes'] / 1024:.1f}KB "
          f"-> pack {stats['pack_bytes'] / 1024:.1f}KB ({stats['ratio']:.2f}x)")

    return repo, c1, c2, cA, cB


def cmd_demo(args: argparse.Namespace) -> int:
    import shutil
    import tempfile
    root = tempfile.mkdtemp(prefix="wellsample-demo-")
    try:
        repo, c1, c2, cA, cB = _build_demo_repo(root)

        ok = True
        if args.verify_byte_exact:
            print("\nbyte-exact checkout verification")
            # check out each commit into a pristine tree and compare hashes
            for label, oid in (("v1", c1), ("v2", c2), ("branchA", cA), ("branchB", cB)):
                fresh = os.path.join(root, "verify-" + label)
                os.makedirs(fresh, exist_ok=True)
                ws = Workspace(fresh, repo.store)
                n = ws.checkout(repo.tree_of(oid))
                problems = ws.verify(repo.tree_of(oid))
                status = "OK" if not problems else f"MISMATCH {problems}"
                print(f"  {label}: {n} files -> {status}")
                ok = ok and not problems

            # exec bit / symlink / empty file spot checks on the last commit
            ws = Workspace(os.path.join(root, "verify-branchB"), repo.store)
            survey = os.path.join(ws.root, "survey")
            checks = {
                "run.sh executable": bool(os.stat(os.path.join(survey, "run.sh")).st_mode & 0o111),
                "symlink target": os.readlink(os.path.join(survey, "latest.segy")) == "well_00.segy",
                "empty file is empty": os.path.getsize(os.path.join(survey, "empty.dat")) == 0,
                "trailing newline kept": open(os.path.join(survey, "notes.txt"), "rb").read() == b"trailing newline\n",
                "no-eol file kept": open(os.path.join(survey, "no_eol.txt"), "rb").read() == b"no trailing newline",
                "nested dir restored": os.path.isfile(os.path.join(survey, "derived", "summary.txt")),
            }
            for name, passed in checks.items():
                print(f"  {name}: {'OK' if passed else 'FAIL'}")
                ok = ok and passed

        print("\nRESULT:", "PASS" if ok else "FAIL")
        return 0 if ok else 1
    finally:
        shutil.rmtree(root, ignore_errors=True)


def cmd_gc(args: argparse.Namespace) -> int:
    repo = Repository(args.repo)
    if not repo.exists():
        print("not a repository", file=sys.stderr)
        return 1
    s = repo.gc(delta_window=args.window)
    print(f"packed {s['objects']} objects: {s['loose_bytes']} -> {s['pack_bytes']} bytes "
          f"({s['ratio']:.2f}x)")
    return 0


def cmd_verify(args: argparse.Namespace) -> int:
    repo = Repository(args.repo)
    if not repo.exists():
        print("not a repository", file=sys.stderr)
        return 1
    problems = repo.verify()
    if problems:
        for p in problems:
            print(f"MISMATCH {p}")
        return 1
    print("working tree matches HEAD byte for byte")
    return 0


def main(argv: Sequence[str] | None = None) -> int:
    p = argparse.ArgumentParser(prog="engine", description="wellsample version-control kernel")
    sub = p.add_subparsers(dest="cmd", required=True)

    d = sub.add_parser("demo", help="run the built-in end-to-end demo")
    d.add_argument("--verify-byte-exact", action="store_true",
                   help="check out every commit and verify it byte for byte")
    d.set_defaults(func=cmd_demo)

    g = sub.add_parser("gc", help="pack loose objects with delta encoding")
    g.add_argument("repo", nargs="?", default=".")
    g.add_argument("--window", type=int, default=MAX_DELTA_CANDIDATES)
    g.set_defaults(func=cmd_gc)

    v = sub.add_parser("verify", help="verify the working tree against HEAD")
    v.add_argument("repo", nargs="?", default=".")
    v.set_defaults(func=cmd_verify)

    args = p.parse_args(argv)
    return args.func(args)


if __name__ == "__main__":
    raise SystemExit(main())
