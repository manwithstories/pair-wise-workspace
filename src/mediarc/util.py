"""Small filesystem helpers shared by every stage.

Writes are atomic (temporary file + ``os.replace``) so an interrupted run can
never leave a half-written manifest or segment artifact that a resume would
mistake for a completed one.
"""

from __future__ import annotations

import hashlib
import json
import os
import tempfile
from pathlib import Path
from typing import Any, BinaryIO

from .errors import MediaError

CHUNK = 1 << 20


def sha256_file(path: str | os.PathLike[str]) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(CHUNK), b""):
            digest.update(chunk)
    return digest.hexdigest()


def sha256_bytes(data: bytes) -> str:
    return hashlib.sha256(data).hexdigest()


def canonical_json(payload: Any) -> bytes:
    return (
        json.dumps(payload, sort_keys=True, separators=(",", ":"), ensure_ascii=False)
        + "\n"
    ).encode("utf-8")


def atomic_write_bytes(path: str | os.PathLike[str], data: bytes) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    fd, tmp = tempfile.mkstemp(prefix=".tmp-", dir=target.parent)
    try:
        with os.fdopen(fd, "wb") as handle:
            handle.write(data)
            handle.flush()
            os.fsync(handle.fileno())
        os.replace(tmp, target)
    except BaseException:
        try:
            os.unlink(tmp)
        except FileNotFoundError:
            pass
        raise


def atomic_write_json(path: str | os.PathLike[str], payload: Any) -> None:
    atomic_write_bytes(path, canonical_json(payload))


def load_json(path: str | os.PathLike[str], *, stage: str = "archive") -> Any:
    try:
        with open(path, "rb") as handle:
            return json.loads(handle.read().decode("utf-8"))
    except FileNotFoundError as exc:
        raise MediaError("io", f"file not found: {path}", stage=stage, path=str(path)) from exc
    except (OSError, UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise MediaError(
            "manifest-corrupt", f"cannot parse JSON: {exc}", stage=stage, path=str(path)
        ) from exc


def copy_prefix(src: str | os.PathLike[str], dst: str | os.PathLike[str], ratio: float) -> int:
    """Copy the first ``ratio`` of a file; returns bytes written."""
    with open(src, "rb") as reader, open(dst, "wb") as writer:
        size = os.fstat(reader.fileno()).st_size
        remaining = int(size * max(0.0, min(1.0, ratio)))
        while remaining:
            chunk = reader.read(min(CHUNK, remaining))
            if not chunk:
                break
            writer.write(chunk)
            remaining -= len(chunk)
        return int(size * ratio)


def flip_packet_bytes(
    src: str | os.PathLike[str],
    dst: str | os.PathLike[str],
    offset: int,
    payload_skip: int = 8,
    length: int = 64,
    mask: int = 0xFF,
) -> None:
    """Copy ``src`` to ``dst`` and scramble one packet's payload region.

    ``offset`` is the packet's container byte position as reported by
    libavformat; ``payload_skip`` skips the container block header so the codec
    payload itself is damaged.  Used to prove that diagnostics land on a source
    byte offset.
    """
    data = bytearray(Path(src).read_bytes())
    start = max(0, offset + payload_skip)
    stop = min(len(data), start + length)
    if stop <= start:
        raise ValueError(f"offset {offset} is outside {src}")
    data[start:stop] = bytes((byte ^ mask) for byte in data[start:stop])
    atomic_write_bytes(dst, bytes(data))


def stream_digest(handle: BinaryIO) -> str:
    digest = hashlib.sha256()
    for chunk in iter(lambda: handle.read(CHUNK), b""):
        digest.update(chunk)
    return digest.hexdigest()
