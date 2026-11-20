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
from typing import Any

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


_QUIET_DEPTH = 0


@_contextlib.contextmanager
def quiet_libav():
    """Silence libav's log callback for expected-failure work.

    Damaged fixtures are decoded on purpose, so the library's error stream is
    noise; diagnoses are captured structurally instead.  The guard is
    re-entrant: nested uses (repair inside self-test, for example) must not
    restore the default callback while an outer guard is still active.  Set
    ``MEDIARC_LIBAV_LOG=1`` to see the raw messages while debugging.
    """
    global _QUIET_DEPTH
    if _os.environ.get("MEDIARC_LIBAV_LOG"):
        yield
        return
    import av as _av

    _QUIET_DEPTH += 1
    if _QUIET_DEPTH == 1:
        capture = _av.logging.Capture()
        capture.__enter__()
        _av.logging.set_level(_av.logging.PANIC)
    try:
        yield
    finally:
        _QUIET_DEPTH -= 1
        if _QUIET_DEPTH == 0:
            _av.logging.restore_default_callback()
