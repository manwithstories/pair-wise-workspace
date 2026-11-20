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


def iter_packets(container: Any, stream: Any, target_seconds: Any = 0):
    """Yield packets of ``stream`` beginning at or before ``target_seconds``.

    Container seeking is advisory: some demuxers ignore it, and a backward seek
    only guarantees "at or before the target *keyframe*".  This iterator
    verifies where the seek actually landed using the first packet it reads;
    if the landing time is past the target it restarts the demuxer and reads
    linearly.  No packet is dropped either way, so callers see a complete
    packet stream and can make sample-exact cuts themselves.
    """
    from fractions import Fraction

    target = Fraction(target_seconds) if not isinstance(target_seconds, Fraction) else target_seconds
    if target > 0:
        try:
            container.seek(int(target / stream.time_base), stream=stream, backward=True)
        except Exception:  # noqa: BLE001 - demuxer may not support seeking
            target = Fraction(0)
    attempt = 0
    while True:
        first = True
        restart = False
        for packet in container.demux(stream):
            if first:
                first = False
                if packet.pts is None and packet.dts is None:
                    # Flush packet at EOF; nothing to verify against.
                    yield packet
                    continue
                when = Fraction(
                    packet.pts if packet.pts is not None else packet.dts
                ) * stream.time_base
                if target > 0 and when > target:
                    restart = True
                    break
            yield packet
        if not restart:
            return
        attempt += 1
        if attempt > 1:
            return
        target = Fraction(0)
        try:
            container.seek(0)
        except Exception:  # noqa: BLE001
            return


import contextlib as _contextlib
import os as _os


@_contextlib.contextmanager
def quiet_libav():
    """Silence libav's log callback for expected-failure work.

    Damaged fixtures are decoded on purpose, so the library's error stream is
    noise; diagnoses are captured structurally instead.  Set
    ``MEDIARC_LIBAV_LOG=1`` to see the raw messages while debugging.
    """
    if _os.environ.get("MEDIARC_LIBAV_LOG"):
        yield
        return
    import av as _av

    try:
        with _av.logging.Capture():
            _av.logging.set_level(_av.logging.PANIC)
            yield
    finally:
        _av.logging.restore_default_callback()
