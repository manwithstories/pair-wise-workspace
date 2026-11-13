"""Media sniffing, normalization contracts, path checks and chunk hashing.

The module deliberately does not use file names or extensions to decide what a
file is.  Callers receive a :class:`MediaKind` derived from the bytes on disk.
"""

from __future__ import annotations

import hashlib
import os
from dataclasses import dataclass
from enum import Enum
from pathlib import Path
from typing import BinaryIO, Iterable


class MediaFamily(str, Enum):
    IMAGE = "image"
    AUDIO = "audio"
    VIDEO = "video"


@dataclass(frozen=True)
class MediaKind:
    family: MediaFamily
    format: str


IMAGE_FORMATS = frozenset({"png", "jpeg", "webp"})
AUDIO_FORMATS = frozenset({"wav", "flac", "ogg"})
VIDEO_FORMATS = frozenset({"mp4", "mov", "mkv"})


@dataclass(frozen=True)
class NormalizationContract:
    """Stable, human-readable contract shared by decoding and verification."""

    image_rgb: str = "RGB uint8, EXIF orientation applied, alpha flattened on white"
    image_hash_grid: int = 32
    audio_mono: str = "mono float32, 16 kHz, silent head/tail trimmed"
    audio_hop_ms: int = 50
    audio_bands: int = 16
    audio_head_crop_ms: int = 250
    video_frames: int = 8
    video_decode_frames: int = 16
    video_crop_seconds: float = 1.0


NORMALIZATION = NormalizationContract()
NORMALIZATION_CONTRACT = {
    "image": {
        "color": "RGB uint8",
        "orientation": "EXIF transposed",
        "alpha": "composited on white",
        "hash_grid": NORMALIZATION.image_hash_grid,
    },
    "audio": {
        "channels": "mono downmix (mean)",
        "sample_rate": 16000,
        "sample_format": "float32",
        "silence": "trim leading/trailing frames below -40 dB relative peak",
        "hop_ms": NORMALIZATION.audio_hop_ms,
        "bands": NORMALIZATION.audio_bands,
        "max_head_crop_ms": NORMALIZATION.audio_head_crop_ms,
    },
    "video": {
        "anchor_frames": NORMALIZATION.video_frames,
        "decode_frames": NORMALIZATION.video_decode_frames,
        "frame_normalization": "image contract",
        "max_end_crop_seconds": NORMALIZATION.video_crop_seconds,
    },
}


class MediaDecodeError(Exception):
    """A classified media decoding failure."""

    def __init__(self, error_type: str, message: str, *, retryable: bool = False):
        super().__init__(message)
        self.error_type = error_type
        self.message = message
        self.retryable = retryable

    def as_dict(self) -> dict[str, object]:
        return {
            "error_type": self.error_type,
            "message": self.message,
            "retryable": self.retryable,
        }


class PathValidationError(ValueError):
    pass


def sniff_kind(header: bytes) -> MediaKind | None:
    """Classify a media family/container from a small leading byte sample."""

    if not header:
        return None

    if header.startswith(b"\x89PNG\r\n\x1a\n"):
        return MediaKind(MediaFamily.IMAGE, "png")
    if header.startswith(b"\xff\xd8\xff"):
        return MediaKind(MediaFamily.IMAGE, "jpeg")
    if len(header) >= 12 and header[:4] == b"RIFF":
        riff_type = header[8:12]
        if riff_type == b"WEBP":
            return MediaKind(MediaFamily.IMAGE, "webp")
        if riff_type == b"WAVE":
            return MediaKind(MediaFamily.AUDIO, "wav")
    if header.startswith(b"fLaC"):
        return MediaKind(MediaFamily.AUDIO, "flac")
    if header.startswith(b"OggS"):
        return MediaKind(MediaFamily.AUDIO, "ogg")
    if header.startswith(b"\x1aE\xdf\xa3"):
        return MediaKind(MediaFamily.VIDEO, "mkv")
    if len(header) >= 12 and header[4:8] == b"ftyp":
        major = header[8:12]
        if major in {b"qt  ", b"qt\x20\x20"}:
            return MediaKind(MediaFamily.VIDEO, "mov")
        # All ISO Base Media brands are treated as MP4 video containers here.
        return MediaKind(MediaFamily.VIDEO, "mp4")
    return None


def sniff_file(path: str | os.PathLike[str], sample_size: int = 64) -> MediaKind | None:
    with open(path, "rb") as stream:
        return sniff_kind(stream.read(sample_size))


def validate_media_path(
    path: str | os.PathLike[str],
    root: str | os.PathLike[str] | None = None,
    *,
    reject_hidden: bool = False,
) -> tuple[Path, str | None]:
    """Validate an existing regular file and return ``(resolved, relative)``.

    Symbolic links are rejected rather than followed.  When ``root`` is given,
    the resolved path must remain inside that root, which also prevents a
    symlinked directory from escaping the corpus.
    """

    raw = Path(os.fspath(path))
    if raw.is_symlink():
        raise PathValidationError(f"symbolic links are not accepted: {raw}")
    try:
        resolved = raw.resolve(strict=True)
    except FileNotFoundError as exc:
        raise PathValidationError(f"file does not exist: {raw}") from exc
    if not resolved.is_file():
        raise PathValidationError(f"not a regular file: {resolved}")
    if resolved.stat().st_size == 0:
        raise PathValidationError(f"zero-byte file: {resolved}")

    relative = None
    if root is not None:
        root_path = Path(os.fspath(root)).resolve(strict=True)
        try:
            rel_path = resolved.relative_to(root_path)
        except ValueError as exc:
            raise PathValidationError(f"path escapes corpus root: {resolved}") from exc
        if reject_hidden and any(part.startswith(".") for part in rel_path.parts):
            raise PathValidationError(f"hidden path is ignored: {rel_path}")
        relative = rel_path.as_posix()
    return resolved, relative


def iter_file_chunks(stream: BinaryIO, chunk_size: int = 1024 * 1024) -> Iterable[bytes]:
    while True:
        chunk = stream.read(chunk_size)
        if not chunk:
            return
        yield chunk


def chunked_sha256(
    path: str | os.PathLike[str], chunk_size: int = 1024 * 1024
) -> str:
    """Hash the full content in fixed-size chunks.

    This hash is an identity/change-detection key, *not* a duplicate decision
    signal.  It is still computed for unchanged-looking files so a replacement
    with the same size and mtime is detected.
    """

    digest = hashlib.sha256()
    with open(path, "rb") as stream:
        for chunk in iter_file_chunks(stream, chunk_size):
            digest.update(chunk)
    return digest.hexdigest()


def classify_exception(error: BaseException) -> str:
    if isinstance(error, MediaDecodeError):
        return error.error_type
    if isinstance(error, PathValidationError):
        return "path_error"
    if isinstance(error, (FileNotFoundError, PermissionError, OSError)):
        return "io_error"
    if isinstance(error, MemoryError):
        return "resource_error"
    return "decode_error"


# Small public aliases for callers that use shorter names.
validate_path = validate_media_path
detect_media_kind = sniff_kind
hash_file_chunks = chunked_sha256
