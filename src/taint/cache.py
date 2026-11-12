"""Content-addressed AST and module-summary cache.

The scanner never imports target code.  Trees are serialized into a compact,
deterministic JSON node graph so a second process can reuse the syntax tree
without invoking tree-sitter again.  Source bytes are retained as base64 to
support non-UTF-8 inputs while keeping the cache file JSON-safe.
"""

from __future__ import annotations

import base64
import hashlib
import json
import os
import tempfile
import threading
from collections import OrderedDict
from dataclasses import dataclass
from pathlib import Path
from typing import Any


_CACHE_VERSION = 1


class CacheError(RuntimeError):
    """Raised when cache construction cannot proceed."""


class AstNode:
    """Small tree-sitter-compatible node used for both live and cached trees."""

    __slots__ = (
        "_source",
        "type",
        "start_byte",
        "end_byte",
        "start_point",
        "end_point",
        "is_named",
        "field_name",
        "children",
        "parent",
    )

    def __init__(
        self,
        node_type: str,
        start_byte: int,
        end_byte: int,
        start_point: tuple[int, int],
        end_point: tuple[int, int],
        named: bool = True,
        field_name: str | None = None,
        source: bytes = b"",
    ) -> None:
        self.type = node_type
        self.start_byte = start_byte
        self.end_byte = end_byte
        self.start_point = start_point
        self.end_point = end_point
        self.is_named = named
        self.field_name = field_name
        self.children: list[AstNode] = []
        self.parent: AstNode | None = None
        self._source = source

    @property
    def text(self) -> bytes:
        return self._source[self.start_byte : self.end_byte]

    @property
    def range(self) -> tuple[tuple[int, int], tuple[int, int]]:
        return (self.start_point, self.end_point)

    @property
    def named_children(self) -> list[AstNode]:
        return [child for child in self.children if child.is_named]

    @property
    def child_count(self) -> int:
        return len(self.children)

    @property
    def named_child_count(self) -> int:
        return len(self.named_children)

    def child(self, index: int) -> AstNode | None:
        if 0 <= index < len(self.children):
            return self.children[index]
        return None

    def named_child(self, index: int) -> AstNode | None:
        named = self.named_children
        if 0 <= index < len(named):
            return named[index]
        return None

    def child_by_field_name(self, name: str) -> AstNode | None:
        for child in self.children:
            if child.field_name == name:
                return child
        return None

    def children_by_field_name(self, name: str) -> list[AstNode]:
        return [child for child in self.children if child.field_name == name]

    def field_name_for_child(self, index: int) -> str | None:
        if 0 <= index < len(self.children):
            return self.children[index].field_name
        return None

    def __repr__(self) -> str:
        return f"<AstNode {self.type} {self.start_point}..{self.end_point}>"


class AstTree:
    __slots__ = ("root_node", "has_error", "language")

    def __init__(self, root_node: AstNode, has_error: bool = False) -> None:
        self.root_node = root_node
        self.has_error = has_error
        self.language = "python"


@dataclass
class CacheStats:
    hit: int = 0
    miss: int = 0
    invalid: int = 0
    parsed: int = 0
    memory_hit: int = 0
    parse_errors: int = 0
    writes: int = 0

    def as_dict(self) -> dict[str, int]:
        return {
            "hit": self.hit,
            "miss": self.miss,
            "invalid": self.invalid,
            "parsed": self.parsed,
            "memory_hit": self.memory_hit,
            "parse_errors": self.parse_errors,
            "writes": self.writes,
        }


@dataclass
class CacheEntry:
    relpath: str
    sha256: str
    mtime_ns: int
    source: bytes
    tree: AstTree
    summary: dict[str, Any] | None = None
    from_disk: bool = False


_language_lock = threading.Lock()
_language: Any = None
_parser_local = threading.local()


def _get_language() -> Any:
    global _language
    if _language is not None:
        return _language
    with _language_lock:
        if _language is not None:
            return _language
        try:
            import tree_sitter
            import tree_sitter_python
        except ImportError as exc:  # pragma: no cover - exercised by packaging
            raise CacheError(
                "tree-sitter and tree-sitter-python are required; "
                "run python -m pip install -r requirements.txt"
            ) from exc

        raw_language = tree_sitter_python.language()
        # tree-sitter >= 0.22 takes an opaque language capsule.  Older 0.21
        # bindings take an integer pointer and a name.
        try:
            language = tree_sitter.Language(raw_language)
        except TypeError:
            try:
                language = tree_sitter.Language(raw_language, "python")
            except TypeError as exc:
                raise CacheError(
                    "incompatible tree-sitter / tree-sitter-python binding; "
                    "use requirements.txt pins"
                ) from exc
        _language = language
        return language


def _get_parser() -> Any:
    parser = getattr(_parser_local, "parser", None)
    if parser is None:
        try:
            from tree_sitter import Parser
        except ImportError as exc:  # pragma: no cover
            raise CacheError("tree-sitter is not installed") from exc
        parser = Parser(_get_language())
        _parser_local.parser = parser
    return parser


def _node_to_data(node: Any, source: bytes) -> dict[str, Any]:
    children: list[dict[str, Any]] = []
    for index, child in enumerate(node.children):
        if not child.is_named:
            continue
        children.append(
            {
                "f": node.field_name_for_child(index),
                "n": _node_to_data(child, source),
            }
        )
    start_row, start_col = node.start_point
    end_row, end_col = node.end_point
    return {
        "t": node.type,
        "s": node.start_byte,
        "e": node.end_byte,
        "sp": [start_row, start_col],
        "ep": [end_row, end_col],
        "c": children,
    }


def _data_to_node(data: dict[str, Any], source: bytes) -> AstNode:
    node = AstNode(
        node_type=data["t"],
        start_byte=int(data["s"]),
        end_byte=int(data["e"]),
        start_point=(int(data["sp"][0]), int(data["sp"][1])),
        end_point=(int(data["ep"][0]), int(data["ep"][1])),
        named=True,
        source=source,
    )
    for child_data in data.get("c", []):
        child = _data_to_node(child_data["n"], source)
        child.field_name = child_data.get("f")
        child.parent = node
        node.children.append(child)
    return node


def _serialize_tree(tree: Any, source: bytes) -> dict[str, Any]:
    return {
        "root": _node_to_data(tree.root_node, source),
        "has_error": bool(tree.root_node.has_error),
    }


def _deserialize_tree(data: dict[str, Any], source: bytes) -> AstTree:
    root = _data_to_node(data["root"], source)
    return AstTree(root, bool(data.get("has_error", False)))


def _atomic_json_write(path: Path, payload: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    fd, temp_name = tempfile.mkstemp(
        prefix=path.name + ".", suffix=".tmp", dir=path.parent
    )
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as handle:
            json.dump(
                payload,
                handle,
                ensure_ascii=False,
                sort_keys=True,
                separators=(",", ":"),
            )
            handle.write("\n")
        os.replace(temp_name, path)
    finally:
        try:
            os.unlink(temp_name)
        except FileNotFoundError:
            pass


class AstCache:
    """Thread-safe LRU + on-disk cache keyed by relative path, sha256 and mtime."""

    def __init__(
        self,
        root: str | os.PathLike[str],
        cache_dir: str | os.PathLike[str] | None = None,
        max_entries: int = 512,
    ) -> None:
        self.root = Path(root).resolve()
        self.cache_dir = (
            Path(cache_dir).resolve()
            if cache_dir
            else (Path.cwd() / ".cache").resolve()
        )
        self.ast_dir = self.cache_dir / "ast"
        self.max_entries = max(8, int(max_entries))
        self.stats = CacheStats()
        self._memory: OrderedDict[str, CacheEntry] = OrderedDict()
        self._lock = threading.RLock()

    def relative_path(self, path: str | os.PathLike[str]) -> str:
        resolved = Path(path).resolve()
        try:
            rel = resolved.relative_to(self.root)
        except ValueError as exc:
            raise CacheError(f"path escapes scan root: {resolved}") from exc
        return rel.as_posix()

    def _disk_path(self, relpath: str) -> Path:
        digest = hashlib.sha256(relpath.encode("utf-8")).hexdigest()
        return self.ast_dir / f"{digest}.json"

    @staticmethod
    def _fingerprint(path: Path) -> tuple[str, int]:
        data = path.read_bytes()
        return hashlib.sha256(data).hexdigest(), path.stat().st_mtime_ns

    def load(self, path: str | os.PathLike[str]) -> CacheEntry:
        path = Path(path).resolve()
        relpath = self.relative_path(path)
        sha256, mtime_ns = self._fingerprint(path)

        with self._lock:
            memory_entry = self._memory.get(relpath)
            if (
                memory_entry is not None
                and memory_entry.sha256 == sha256
                and memory_entry.mtime_ns == mtime_ns
            ):
                self._memory.move_to_end(relpath)
                self.stats.hit += 1
                self.stats.memory_hit += 1
                return memory_entry

        disk_path = self._disk_path(relpath)
        disk_entry: CacheEntry | None = None
        invalid = False
        if disk_path.is_file():
            try:
                payload = json.loads(disk_path.read_text(encoding="utf-8"))
                if (
                    payload.get("version") == _CACHE_VERSION
                    and payload.get("relpath") == relpath
                    and payload.get("sha256") == sha256
                    and int(payload.get("mtime_ns", -1)) == mtime_ns
                ):
                    source = base64.b64decode(payload["source_b64"])
                    tree = _deserialize_tree(payload["ast"], source)
                    disk_entry = CacheEntry(
                        relpath=relpath,
                        sha256=sha256,
                        mtime_ns=mtime_ns,
                        source=source,
                        tree=tree,
                        summary=payload.get("summary"),
                        from_disk=True,
                    )
                else:
                    invalid = True
            except (OSError, ValueError, KeyError, TypeError, json.JSONDecodeError):
                invalid = True

        if disk_entry is not None:
            with self._lock:
                self.stats.hit += 1
                self._remember(disk_entry)
            return disk_entry

        source = path.read_bytes()
        if invalid:
            with self._lock:
                self.stats.invalid += 1
        else:
            with self._lock:
                self.stats.miss += 1

        tree = _get_parser().parse(source)
        ast_tree = _deserialize_tree(_serialize_tree(tree, source), source)
        entry = CacheEntry(
            relpath=relpath,
            sha256=sha256,
            mtime_ns=mtime_ns,
            source=source,
            tree=ast_tree,
            summary=None,
            from_disk=False,
        )
        with self._lock:
            self.stats.parsed += 1
            if ast_tree.has_error:
                self.stats.parse_errors += 1
            self._remember(entry)
            self._persist(entry)
        return entry

    def _remember(self, entry: CacheEntry) -> None:
        self._memory[entry.relpath] = entry
        self._memory.move_to_end(entry.relpath)
        while len(self._memory) > self.max_entries:
            self._memory.popitem(last=False)

    def _persist(self, entry: CacheEntry) -> None:
        payload = {
            "version": _CACHE_VERSION,
            "relpath": entry.relpath,
            "sha256": entry.sha256,
            "mtime_ns": entry.mtime_ns,
            "source_b64": base64.b64encode(entry.source).decode("ascii"),
            "ast": _tree_to_data(entry.tree),
            "summary": entry.summary,
        }
        _atomic_json_write(self._disk_path(entry.relpath), payload)
        self.stats.writes += 1

    def store_summary(self, entry: CacheEntry, summary: dict[str, Any]) -> None:
        with self._lock:
            entry.summary = summary
            if entry.relpath in self._memory:
                self._memory[entry.relpath] = entry
                self._memory.move_to_end(entry.relpath)
            self._persist(entry)

    def get_summary(self, entry: CacheEntry) -> dict[str, Any] | None:
        with self._lock:
            return entry.summary

    def clear_memory(self) -> None:
        with self._lock:
            self._memory.clear()


def _tree_to_data(tree: AstTree) -> dict[str, Any]:
    """Serialize an already converted tree without touching tree-sitter."""

    def node_data(node: AstNode) -> dict[str, Any]:
        return {
            "t": node.type,
            "s": node.start_byte,
            "e": node.end_byte,
            "sp": list(node.start_point),
            "ep": list(node.end_point),
            "c": [
                {"f": child.field_name, "n": node_data(child)}
                for child in node.children
            ],
        }

    return {"root": node_data(tree.root_node), "has_error": tree.has_error}
