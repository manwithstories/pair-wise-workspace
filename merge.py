"""Three-way merge over trees, with add/delete/rename edge-case handling.

Given a common base plus two sides, produce a merged tree and a list of
conflicts. A path may be renamed on one side; renames are detected by
content hash so a move plus an edit still lines up with its counterpart.
"""
from __future__ import annotations

from object_store import ObjectStore

__all__ = ["MergeResult", "Conflict", "three_way_merge", "detect_renames",
           "render_conflict", "OURS_MARKER", "BASE_MARKER", "THEIRS_MARKER"]

OURS_MARKER = b"<<<<<<< ours"
BASE_MARKER = b"||||||| base"
SEPARATOR = b"======="
THEIRS_MARKER = b">>>>>>> theirs"


class Conflict:
    """One unresolved path."""

    __slots__ = ("path", "kind", "ours", "theirs", "base")

    def __init__(self, path: str, kind: str, ours: str | None,
                 theirs: str | None, base: str | None) -> None:
        self.path = path
        self.kind = kind
        self.ours = ours
        self.theirs = theirs
        self.base = base

    def __repr__(self) -> str:
        return f"Conflict({self.path!r}, {self.kind})"


class MergeResult:
    __slots__ = ("entries", "conflicts", "renames")

    def __init__(self, entries: dict[str, tuple[int, str]],
                 conflicts: list[Conflict], renames: dict[str, str]) -> None:
        self.entries = entries        # path -> (mode, oid)
        self.conflicts = conflicts
        self.renames = renames        # old path -> new path

    @property
    def clean(self) -> bool:
        return not self.conflicts

    def __repr__(self) -> str:
        return f"MergeResult({len(self.entries)} entries, {len(self.conflicts)} conflicts)"


def _flatten(store: ObjectStore, tree_oid: str | None, prefix: str = "",
             out: dict[str, tuple[int, str]] | None = None) -> dict[str, tuple[int, str]]:
    """Expand a tree into path -> (mode, oid) for every non-tree entry."""
    if out is None:
        out = {}
    if not tree_oid:
        return out
    for e in store.read_tree(tree_oid):
        path = f"{prefix}{e.name.decode('utf-8', 'surrogateescape')}"
        if e.is_dir:
            _flatten(store, e.oid, path + "/", out)
        else:
            out[path] = (e.mode, e.oid)
    return out


def detect_renames(base: dict[str, tuple[int, str]],
                   side: dict[str, tuple[int, str]]) -> dict[str, str]:
    """Map old path -> new path for entries whose content moved.

    Matches on (mode, oid) so an unmodified file that moved is recognised even
    when the two sides renamed it differently.
    """
    by_content: dict[tuple[int, str], list[str]] = {}
    for path, (mode, oid) in base.items():
        by_content.setdefault((mode, oid), []).append(path)
    renames: dict[str, str] = {}
    for new_path, (mode, oid) in side.items():
        if new_path in base:
            continue
        for old in by_content.get((mode, oid), ()):
            if old not in side:          # old path was removed on this side
                renames[old] = new_path
                break
    return renames


def _same(a: tuple[int, str] | None, b: tuple[int, str] | None) -> bool:
    return a is not None and b is not None and a[0] == b[0] and a[1] == b[1]


def three_way_merge(store: ObjectStore, base_tree: str | None,
                    ours_tree: str | None, theirs_tree: str | None) -> MergeResult:
    """Merge ``ours`` and ``theirs`` relative to ``base``.

    Handles the three boundaries the archive cares about:
      * both sides add the same path with different content -> add/add conflict
      * one side deletes while the other modifies -> modify/delete conflict
      * a path renamed on one side and modified on the other -> rename/modify
    """
    base = _flatten(store, base_tree)
    ours = _flatten(store, ours_tree)
    theirs = _flatten(store, theirs_tree)

    our_renames = detect_renames(base, ours)
    their_renames = detect_renames(base, theirs)

    # Both sides must speak the same path vocabulary. If either side renamed a
    # path, evaluate that path under its new name on *both* sides, so a rename
    # on one side and no change on the other merges cleanly instead of looking
    # like an add against a delete.
    rename_map = dict(our_renames)
    for old, new_path in their_renames.items():
        rename_map.setdefault(old, new_path)

    def normalize(side: dict[str, tuple[int, str]]) -> dict[str, tuple[int, str]]:
        out: dict[str, tuple[int, str]] = {}
        for path, value in side.items():
            target = rename_map.get(path, path)
            out[target] = value
        return out

    def normalize_base(path: str, value: tuple[int, str] | None):
        if value is None:
            return None
        target = rename_map.get(path, path)
        return target, value

    ours_n = normalize(ours)
    theirs_n = normalize(theirs)
    base_n: dict[str, tuple[int, str]] = {}
    for path, value in base.items():
        target, val = normalize_base(path, value)
        base_n[target] = val

    all_paths = set(base_n) | set(ours_n) | set(theirs_n)
    conflicts_pre: list[Conflict] = []

    # A side that did not follow a rename still holds the content under the old
    # name. Surface that as a conflict instead of silently discarding the edit.
    for old_path, new_path in rename_map.items():
        other_base = base.get(old_path)
        if other_base is None:
            continue
        for side, side_n, label in ((ours, ours_n, "ours"), (theirs, theirs_n, "theirs")):
            entry = side.get(old_path)
            if entry is None or new_path in side:
                continue          # this side did follow the rename
            if not _same(other_base, entry):
                conflicts_pre.append(
                    Conflict(new_path, "rename/modify", new_path if label == "ours" else None,
                             None if label == "ours" else new_path, old_path))
    merged: dict[str, tuple[int, str]] = {}
    conflicts: list[Conflict] = []

    for path in sorted(all_paths):
        b = base_n.get(path)
        o = ours_n.get(path)
        t = theirs_n.get(path)

        if _same(o, t):
            if o is not None:
                merged[path] = o
            continue

        if o is None and t is None:
            continue                      # removed on both sides

        # Absent on a side that never had the path in base means that side
        # simply did not touch it; absent where base had it means a deletion.
        if o is None and t is not None:
            if b is None:
                merged[path] = t          # added by them only
                continue
            if _same(b, t):
                continue                  # they did not touch it; deletion stands
            conflicts.append(Conflict(path, "delete/modify", None, path, path))
            continue
        if t is None and o is not None:
            if b is None:
                merged[path] = o          # added by us only
                continue
            if _same(b, o):
                continue                  # we did not touch it; deletion stands
            conflicts.append(Conflict(path, "modify/delete", path, None, path))
            continue

        if o is None or t is None:
            continue

        if b is None:
            # both sides added this path with different content
            conflicts.append(Conflict(path, "add/add", path, path, None))
            continue
        if not _same(b, o) and not _same(b, t):
            if _same(o, t):
                merged[path] = o
            else:
                conflicts.append(Conflict(path, "content", path, path, path))
            continue
        if not _same(b, o):
            merged[path] = o
        elif not _same(b, t):
            merged[path] = t
        else:
            merged[path] = o

    seen = {(c.path, c.kind) for c in conflicts}
    for c in conflicts_pre:
        if (c.path, c.kind) not in seen:
            conflicts.append(c)

    renames = dict(our_renames)
    renames.update(their_renames)
    return MergeResult(merged, conflicts, renames)


def render_conflict(path: str, ours: bytes | None, base: bytes | None,
                    theirs: bytes | None) -> bytes:
    """Build a conflict file keeping all three original byte sequences."""
    def labelled(marker: bytes, data: bytes | None) -> bytes:
        body = data if data is not None else b""
        return marker + b"\n" + body

    out = bytearray()
    out += labelled(OURS_MARKER, ours)
    if base is not None:
        out += b"\n" + BASE_MARKER + b"\n" + base
    out += b"\n" + SEPARATOR + b"\n"
    out += theirs if theirs is not None else b""
    out += b"\n" + THEIRS_MARKER + b"\n"
    return bytes(out)
