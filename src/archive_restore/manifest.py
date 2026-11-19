"""Manifest parsing facade.

Auto-detects HLS vs DASH and delegates to the dedicated parsers.  The shared
data model and path-security helpers live in :mod:`manifest_common`; they are
re-exported here so the rest of the package keeps a single import surface.
"""

from __future__ import annotations

from pathlib import Path

from .dash import parse_dash as _parse_dash
from .hls import parse_hls as _parse_hls
from .manifest_common import (
    ManifestError,
    ParsedManifest,
    Representation,
    SegmentRef,
    safe_resolve,
)

__all__ = [
    "ManifestError",
    "ParsedManifest",
    "Representation",
    "SegmentRef",
    "find_manifests",
    "parse_dash",
    "parse_hls",
    "parse_manifest",
    "safe_resolve",
]


def parse_hls(path: Path, root: Path | None = None) -> ParsedManifest:
    root = (root or path.parent).resolve()
    return _parse_hls(path, root)


def parse_dash(path: Path, root: Path | None = None) -> ParsedManifest:
    root = (root or path.parent).resolve()
    return _parse_dash(path, root)


def parse_manifest(path: Path, root: Path | None = None) -> ParsedManifest:
    """Auto-detect HLS vs DASH and parse into the unified representation model."""
    head = path.read_text(errors="replace")[:512].lstrip()
    if head.startswith("<?xml") or head.startswith("<MPD") or path.suffix == ".mpd":
        return parse_dash(path, root)
    return parse_hls(path, root)


def find_manifests(root: Path) -> list[Path]:
    """List entry manifests (m3u8 / mpd), skipping nested media playlists."""
    return sorted(p for p in root.rglob("*") if p.suffix in (".m3u8", ".mpd")
                  and "media" not in p.name.lower().replace("_", "-"))
