"""Streaming output finalization and byte-level validation.

The final WAV is assembled in bounded blocks so peak memory stays flat for a
60-minute programme, validated (frame count and digest) while still a temp
file, and only then atomically renamed into place.
"""

from __future__ import annotations

import hashlib
import os
from pathlib import Path

import numpy as np
import soundfile as sf

from .errors import MediaBatchError
from .index import ChunkRecord, sha256_file_region

BLOCK_BYTES = 1 << 20  # 1 MiB of s16 mono = ~11 s of audio


def sha256_streaming(path: str | Path) -> str:
    h = hashlib.sha256()
    with open(path, "rb") as fh:
        while True:
            chunk = fh.read(BLOCK_BYTES)
            if not chunk:
                break
            h.update(chunk)
    return h.hexdigest()


def finalize_wav(pcm_path: str | Path, final_path: str | Path,
                 sample_rate: int) -> str:
    """Convert raw s16le PCM to WAV atomically; return the WAV's sha256.

    The temp file is written, then re-opened to confirm its frame count and to
    digest it; only a validated file replaces the target.
    """
    pcm_path, final_path = Path(pcm_path), Path(final_path)
    final_path.parent.mkdir(parents=True, exist_ok=True)
    tmp = final_path.with_name(final_path.name + ".tmp")
    expected_frames = pcm_path.stat().st_size // 2 if pcm_path.exists() else 0
    try:
        with sf.SoundFile(str(tmp), "w", samplerate=sample_rate, channels=1,
                          subtype="PCM_16", format="WAV") as out:
            if pcm_path.exists():
                with open(pcm_path, "rb") as fh:
                    while True:
                        raw = fh.read(BLOCK_BYTES)
                        if not raw:
                            break
                        out.write(np.frombuffer(raw, dtype=np.int16))
        info = sf.info(str(tmp))
        if info.frames != expected_frames or info.channels != 1:
            raise MediaBatchError(
                "output_validation_failed",
                f"finalized WAV has {info.frames} frames, expected {expected_frames}",
                str(tmp), int(info.frames * 2))
        with open(tmp, "rb") as fh:
            os.fsync(fh.fileno())
        digest = sha256_streaming(tmp)
        os.replace(tmp, final_path)
    except MediaBatchError:
        tmp.unlink(missing_ok=True)
        raise
    except (OSError, sf.LibsndfileError) as exc:
        tmp.unlink(missing_ok=True)
        raise MediaBatchError("io_error", f"finalize failed: {exc}",
                              str(final_path), 0)
    return digest


def locate_chunk_damage(records: list[ChunkRecord], wav_path: str | Path,
                        pcm_path: str | Path, data_offset: int
                        ) -> tuple[ChunkRecord | None, int]:
    """Return (first damaged chunk, exact WAV byte offset) or (None, -1).

    Each committed chunk's region digest is checked first; the first mismatch
    is then resolved to a byte by comparing against the checkpointed PCM, which
    the WAV payload mirrors one-to-one.
    """
    if not Path(wav_path).exists():
        return (records[0] if records else None), 0
    size = Path(wav_path).stat().st_size
    for rec in records:
        start = data_offset + rec.output_offset
        if start + rec.output_length > size:
            return rec, start
        digest = sha256_file_region(wav_path, start, rec.output_length)
        if digest == rec.output_sha256:
            continue
        with open(wav_path, "rb") as fh:
            fh.seek(start)
            got = fh.read(rec.output_length)
        ref = b""
        if Path(pcm_path).exists():
            with open(pcm_path, "rb") as fh:
                fh.seek(rec.output_offset)
                ref = fh.read(rec.output_length)
        for i, (a, b) in enumerate(zip(got, ref)):
            if a != b:
                return rec, start + i
        return rec, start
    return None, -1
