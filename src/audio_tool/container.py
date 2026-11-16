"""Container-level integrity checks for WAV ingest.

libsndfile clamps a truncated ``data`` chunk to the frames that actually
exist, so a half-copied file looks perfectly healthy through its header.  For
an archival tool that is the exact failure that must be caught, so the RIFF
chunk structure is walked explicitly before any samples are trusted.
"""

from __future__ import annotations

from pathlib import Path

from .model import InputFormatError

#: Minimum ``fmt `` payload this checker needs (channels at 2, bits at 14).
_FMT_MIN = 16


def check_wav_integrity(path: Path) -> None:
    """Require the advertised data chunk to fit inside the real file.

    Also verifies the data size is a whole number of frames, using the frame
    size derived from the ``fmt `` chunk.  Non-RIFF containers (FLAC) return
    immediately; their decoder detects truncation itself.
    """

    path = Path(path)
    raw_size = path.stat().st_size
    try:
        with open(path, "rb") as handle:
            header = handle.read(12)
            if len(header) < 12 or header[0:4] != b"RIFF" or header[8:12] != b"WAVE":
                return
            declared = int.from_bytes(header[4:8], "little")
            if declared + 8 > raw_size + 1:  # RIFF allows 1 byte of pad
                raise InputFormatError(
                    f"truncated WAV: header declares {declared + 8} bytes, "
                    f"file holds {raw_size}",
                    details={"path": str(path), "declared_bytes": declared + 8,
                             "actual_bytes": raw_size})
            block_align = 1
            while True:
                chunk_header = handle.read(8)
                if len(chunk_header) < 8:
                    break
                chunk_id = chunk_header[0:4]
                chunk_size = int.from_bytes(chunk_header[4:8], "little")
                payload_start = handle.tell()
                if chunk_id == b"fmt " and chunk_size >= _FMT_MIN:
                    fmt = handle.read(chunk_size)
                    channels = int.from_bytes(fmt[2:4], "little")
                    bits = int.from_bytes(fmt[14:16], "little")
                    block_align = max(1, channels * (bits // 8 or 1))
                elif chunk_id == b"data":
                    _check_data_chunk(path, chunk_size, raw_size - payload_start,
                                      block_align)
                    return
                handle.seek(payload_start + chunk_size + (chunk_size & 1))
    except OSError as exc:
        raise InputFormatError(f"cannot read container: {exc}",
                               details={"path": str(path)}) from exc


def _check_data_chunk(path: Path, declared: int, available: int,
                      block_align: int) -> None:
    if declared > available:
        raise InputFormatError(
            f"truncated WAV data chunk: declares {declared} bytes, "
            f"only {available} present",
            details={"path": str(path), "declared_bytes": declared,
                     "actual_bytes": available})
    if block_align > 1 and declared % block_align:
        raise InputFormatError(
            f"data chunk size {declared} is not a whole number of "
            f"{block_align}-byte frames", details={"path": str(path)})
