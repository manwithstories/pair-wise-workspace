"""Shared manifest data model, path security and codec helpers."""

from __future__ import annotations

import posixpath
import re
from dataclasses import dataclass, field
from pathlib import Path

from .errors import ErrorCollector

_URI_SCHEME = re.compile(r"^[a-zA-Z][a-zA-Z0-9+.-]*:")
_VIDEO_CODECS = ("avc", "hvc", "hev", "vp0", "vp9", "av01", "mp4v")
_AUDIO_CODECS = ("mp4a", "opus", "ac-3", "ec-3", "flac")


class ManifestError(ValueError):
    """Raised for references that must never be opened."""


@dataclass
class SegmentRef:
    """One segment entry resolved relative to its manifest directory."""

    index: int
    path: Path
    duration_us: int | None = None
    start_us: int | None = None
    byte_offset: int | None = None
    byte_length: int | None = None
    uri: str = ""
    position: int = 0
    disorder: bool = False


@dataclass
class Representation:
    rep_id: str
    kind: str  # "video" | "audio"
    bandwidth: int
    codec: str
    base_dir: Path
    segments: list[SegmentRef] = field(default_factory=list)
    target_duration_us: int | None = None
    wrap_points: set[int] = field(default_factory=set)


@dataclass
class ParsedManifest:
    kind: str
    path: Path
    root_dir: Path
    representations: list[Representation]
    target_duration_us: int | None
    errors: ErrorCollector = field(default_factory=ErrorCollector)

    def has_fatal(self) -> bool:
        return self.errors.has_fatal()


def safe_resolve(root_dir: Path, base_dir: Path, uri: str) -> Path:
    """Resolve *uri* under *base_dir*, guaranteeing the result stays in root."""
    if _URI_SCHEME.match(uri):
        raise ManifestError(f"absolute URI reference rejected: {uri}")
    if uri.startswith("/"):
        raise ManifestError(f"absolute path reference rejected: {uri}")
    normalized = posixpath.normpath(uri)
    parts = normalized.split("/")
    if parts[0] == ".." and normalized != ".":
        raise ManifestError(f"directory traversal rejected: {uri}")
    root = root_dir.resolve()
    candidate = (base_dir / normalized).resolve()
    if candidate != root and root not in candidate.parents:
        raise ManifestError(f"reference outside manifest root rejected: {uri}")
    return candidate


def codec_kind(codec: str, fallback: str = "video") -> str:
    head = codec.lower()
    if head.startswith(_VIDEO_CODECS):
        return "video"
    if head.startswith(_AUDIO_CODECS):
        return "audio"
    return fallback


# Backwards-compatible private alias used by the parser submodules.
_codec_kind = codec_kind
