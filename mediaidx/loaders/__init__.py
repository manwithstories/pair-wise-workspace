"""Decode registry shared by indexing, verification and inspection."""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from mediaidx.canonical import (
    MediaDecodeError,
    MediaFamily,
    MediaKind,
    classify_exception,
    sniff_file,
    sniff_kind,
    validate_media_path,
)


LOADER_VERSION = "2026.11.3"


@dataclass
class LoadedMedia:
    kind: MediaKind
    normalized: Any
    loader_name: str
    loader_version: str
    metadata: dict[str, Any] = field(default_factory=dict)


@dataclass(frozen=True)
class DecodeFailure:
    kind: MediaKind | None
    error_type: str
    message: str
    loader_name: str = ""

    def as_dict(self) -> dict[str, Any]:
        return {
            "status": "error",
            "family": self.kind.family.value if self.kind else "unknown",
            "format": self.kind.format if self.kind else "unknown",
            "error_type": self.error_type,
            "message": self.message,
            "loader_name": self.loader_name,
        }


LoaderFn = Callable[[Path, bytes | None], Any]


class DecodeRegistry:
    """A small explicit registry; no import-time plugin magic is used."""

    def __init__(self) -> None:
        self._loaders: dict[tuple[MediaFamily, str], tuple[str, LoaderFn, dict[str, Any]]] = {}

    def register(
        self,
        family: MediaFamily,
        formats: tuple[str, ...] | list[str] | set[str],
        name: str,
        loader: LoaderFn,
        *,
        metadata: dict[str, Any] | None = None,
    ) -> None:
        for format_name in formats:
            self._loaders[(family, format_name)] = (name, loader, metadata or {})

    def supports(self, kind: MediaKind) -> bool:
        return (kind.family, kind.format) in self._loaders

    def decode_bytes(
        self, data: bytes, *, kind: MediaKind | None = None, path: Path | None = None
    ) -> LoadedMedia | DecodeFailure:
        if kind is None:
            kind = sniff_kind(data[:64])
        if kind is None:
            return DecodeFailure(None, "unsupported_format", "unrecognised media magic")
        entry = self._loaders.get((kind.family, kind.format))
        if entry is None:
            return DecodeFailure(kind, "unsupported_format", f"no loader for {kind.format}")
        name, loader, extra = entry
        try:
            normalized = loader(Path(path or "<memory>"), data)
            metadata = dict(extra)
            if hasattr(normalized, "metadata"):
                metadata.update(normalized.metadata())
            return LoadedMedia(kind, normalized, name, LOADER_VERSION, metadata)
        except MediaDecodeError as exc:
            return DecodeFailure(kind, exc.error_type, exc.message, name)
        except Exception as exc:  # decode isolation needs a classified boundary
            return DecodeFailure(kind, classify_exception(exc), str(exc), name)

    def decode_path(self, path: str | os.PathLike[str]) -> LoadedMedia | DecodeFailure:
        raw = Path(os.fspath(path))
        try:
            resolved, _ = validate_media_path(raw)
        except Exception as exc:
            return DecodeFailure(None, classify_exception(exc), str(exc))
        kind = sniff_file(resolved, 64)
        if kind is None:
            return DecodeFailure(None, "unsupported_format", "unrecognised media magic")
        entry = self._loaders.get((kind.family, kind.format))
        if entry is None:
            return DecodeFailure(kind, "unsupported_format", f"no loader for {kind.format}")
        name, loader, extra = entry
        try:
            normalized = loader(resolved, None)
            metadata = dict(extra)
            if hasattr(normalized, "metadata"):
                metadata.update(normalized.metadata())
            return LoadedMedia(kind, normalized, name, LOADER_VERSION, metadata)
        except MediaDecodeError as exc:
            return DecodeFailure(kind, exc.error_type, exc.message, name)
        except Exception as exc:
            return DecodeFailure(kind, classify_exception(exc), str(exc), name)

    def decode_for_fingerprint(
        self, path: str | os.PathLike[str], expected: MediaKind | None = None
    ) -> LoadedMedia | DecodeFailure:
        result = self.decode_path(path)
        if expected is not None and not isinstance(result, DecodeFailure):
            if result.kind != expected:
                return DecodeFailure(
                    result.kind,
                    "magic_mismatch",
                    f"expected {expected}, observed {result.kind}",
                )
        return result

    @property
    def loader_names(self) -> list[str]:
        return sorted({name for name, _, _ in self._loaders.values()})


REGISTRY = DecodeRegistry()


def register_builtin_loaders() -> None:
    if getattr(REGISTRY, "_builtins_registered", False):
        return
    from .audio_loader import AUDIO_LOADER
    from .image_loader import IMAGE_LOADER
    from .video_loader import VIDEO_LOADER

    IMAGE_LOADER.register_into(REGISTRY)
    AUDIO_LOADER.register_into(REGISTRY)
    VIDEO_LOADER.register_into(REGISTRY)
    REGISTRY._builtins_registered = True  # type: ignore[attr-defined]


register_builtin_loaders()


def decode_path(path: str | os.PathLike[str]) -> LoadedMedia | DecodeFailure:
    return REGISTRY.decode_path(path)


def decode_bytes(data: bytes, path: Path | None = None) -> LoadedMedia | DecodeFailure:
    return REGISTRY.decode_bytes(data, path=path)


decode_file = decode_path
