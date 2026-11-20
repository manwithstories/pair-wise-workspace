"""Verification: seek sampling, offset/hash checks, resume equivalence.

``verify`` answers the operational question "did the repair actually work?"
without trusting the repair run:

1. **Index sanity.**  Every keyframe row must point inside the output file
   and at a byte range that exists; keyframe times must be non-decreasing.
2. **Chunk integrity.**  Each chunk's byte range in the repaired file is
   re-hashed and compared with the digest stored in the index, so tampering
   or a torn write is caught at the exact chunk.
3. **Random seek.**  For a spread of keyframe times it seeks the repaired
   file through PyAV, decodes the first frame, and checks it lands at (or
   just after) the requested keyframe - the definition of random access.
4. **Full-file hash.**  The output SHA-256 must equal the one recorded at
   repair time.
5. **Cross-run equivalence.**  With ``--compare-index`` (typically the index
   of an uninterrupted run) every chunk digest and the whole-file hash must
   also match, which is how a resumed run is proven identical to a full one.

Only the index and the repaired file are read here; the source path is used
solely to label the report and is never opened for writing.
"""

from __future__ import annotations

import hashlib
import random
from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path

import av

from . import probe as probe_mod
from .errors import (
    EXIT_INTERNAL,
    EXIT_OK,
    EXIT_RESUME_MISMATCH,
    IndexIncompatibleError,
    MediaBridgeError,
)
from .index_read import IndexReader


@dataclass
class VerifyConfig:
    source: Path
    index_path: Path
    output: Path | None = None
    sample_targets: int = 12
    checkpoint_path: Path | None = None
    reseed: int = 20261120
    #: Optional second index (e.g. from a full, uninterrupted run).  When
    #: given, every chunk's digest and the whole-file SHA-256 must match,
    #: which is how a resumed run is proven equivalent to a complete one.
    compare_index: Path | None = None


@dataclass
class SeekCheck:
    track_index: int
    target_seconds: float
    requested_bytes: int
    decoded_seconds: float | None = None
    decoded_packet: int | None = None
    within_tolerance: bool = False
    index_offset_valid: bool = False
    note: str = ""

    def as_dict(self) -> dict:
        return {
            "track_index": self.track_index,
            "target_seconds": round(self.target_seconds, 6),
            "requested_bytes": self.requested_bytes,
            "decoded_seconds": None if self.decoded_seconds is None else round(self.decoded_seconds, 6),
            "decoded_packet": self.decoded_packet,
            "within_tolerance": self.within_tolerance,
            "index_offset_valid": self.index_offset_valid,
            "note": self.note,
        }


@dataclass
class VerifySummary:
    command: str = "verify"
    status: str = "ok"
    exit_code: int = EXIT_OK
    source: str = ""
    output: str = ""
    index: str = ""
    index_rows: int = 0
    keyframes: int = 0
    chunks_checked: int = 0
    chunks_expected: int = 0
    chunk_mismatches: list[int] = field(default_factory=list)
    seek_checks: int = 0
    seek_checks_passed: int = 0
    seeks: list[dict] = field(default_factory=list)
    output_sha256: str = ""
    recorded_sha256: str = ""
    hash_matches: bool | None = None
    first_error_packet: int | None = None
    resume_start_packet: int | None = None
    resume_seconds: float | None = None
    reference_index: str = ""
    reference_chunks: int | None = None
    reference_sha256: str = ""
    chunk_digests_match: bool | None = None
    timing_error_seconds: float = 0.0
    warnings: list[str] = field(default_factory=list)
    error: dict | None = None

    def as_dict(self) -> dict:
        return {
            "command": self.command,
            "status": self.status,
            "exit_code": self.exit_code,
            "source": self.source,
            "output": self.output,
            "index": self.index,
            "index_rows": self.index_rows,
            "keyframes": self.keyframes,
            "chunks_checked": self.chunks_checked,
            "chunks_expected": self.chunks_expected,
            "chunk_mismatches": self.chunk_mismatches,
            "seek_checks": self.seek_checks,
            "seek_checks_passed": self.seek_checks_passed,
            "seeks": self.seeks,
            "output_sha256": self.output_sha256,
            "recorded_sha256": self.recorded_sha256,
            "hash_matches": self.hash_matches,
            "first_error_packet": self.first_error_packet,
            "resume_start_packet": self.resume_start_packet,
            "resume_seconds": self.resume_seconds,
            "reference_index": self.reference_index,
            "reference_chunks": self.reference_chunks,
            "reference_sha256": self.reference_sha256,
            "chunk_digests_match": self.chunk_digests_match,
            "timing_error_seconds": round(self.timing_error_seconds, 6),
            "warnings": self.warnings,
            "error": self.error,
        }


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for block in iter(lambda: handle.read(probe_mod.HASH_CHUNK), b""):
            digest.update(block)
    return digest.hexdigest()


class Verifier:
    def __init__(self, config: VerifyConfig):
        self.config = config
        self.summary = VerifySummary(
            source=str(config.source),
            index=str(config.index_path),
        )

    def run(self) -> VerifySummary:
        try:
            self._run()
        except MediaBridgeError as exc:
            self.summary.status = "failed"
            self.summary.exit_code = exc.exit_code
            self.summary.error = exc.as_dict()
        except Exception as exc:
            self.summary.status = "failed"
            self.summary.exit_code = EXIT_INTERNAL
            self.summary.error = {"code": "internal_error",
                                  "message": f"{type(exc).__name__}: {exc}",
                                  "exit_code": EXIT_INTERNAL}
        return self.summary

    def _run(self) -> None:
        cfg = self.config
        with IndexReader(cfg.index_path) as reader:
            info = reader.info()
            output_path = self._resolve_output(cfg, info)
            self.summary.output = str(output_path)
            self.summary.index_rows = info.sample_count
            if not output_path.exists():
                raise IndexIncompatibleError(
                    f"indexed output does not exist: {output_path}",
                    context={"output": str(output_path)},
                )
            self._check_offsets(reader, output_path)
            self._check_chunks(reader, output_path)
            self._check_seeks(reader, output_path)
            self._check_hash(reader, output_path)
            if cfg.compare_index is not None:
                self._compare_reference(reader, Path(cfg.compare_index))

    # -- helpers ---------------------------------------------------------
    @staticmethod
    def _resolve_output(cfg: VerifyConfig, info) -> Path:
        if cfg.output is not None:
            return Path(cfg.output)
        recorded = info.output.get("path")
        if not recorded:
            raise IndexIncompatibleError("index does not name an output file")
        return Path(recorded)

    def _check_offsets(self, reader: IndexReader, output: Path) -> None:
        size = output.stat().st_size
        rows = reader.samples()
        previous: dict[int, float] = {}
        for row in rows:
            offset = int(row["byte_offset"])
            length = int(row["byte_size"])
            if offset < 0 or length <= 0 or offset + length > size:
                raise IndexIncompatibleError(
                    "index row points outside the repaired file",
                    context={"offset": offset, "size": length, "file_size": size,
                             "packet": row["global_packet"]},
                )
            if row["keyframe"]:
                seconds = float(Fraction(row["time_seconds"]))
                last = previous.get(row["track_index"])
                if last is not None and seconds < last:
                    raise IndexIncompatibleError(
                        "keyframe times are not monotonic",
                        context={"track": row["track_index"], "time": seconds, "previous": last},
                    )
                previous[row["track_index"]] = seconds
                self.summary.keyframes += 1
        meta = reader.meta()
        recorded_first = meta.get("first_error_packet")
        if recorded_first not in (None, ""):
            self.summary.first_error_packet = int(recorded_first)
        recorded_resume = meta.get("resume_start_packet")
        if recorded_resume not in (None, ""):
            self.summary.resume_start_packet = int(recorded_resume)
        # resume_seconds is derivable from the cursor: the start time of the
        # first chunk written after the committed boundary.
        if self.summary.resume_start_packet is not None:
            for chunk in reader.chunks():
                if int(chunk["first_global_packet"]) >= self.summary.resume_start_packet:
                    self.summary.resume_seconds = float(Fraction(chunk["start_seconds"]))
                    break

    def _check_chunks(self, reader: IndexReader, output: Path) -> None:
        stored = reader.chunks()
        self.summary.chunks_expected = len(stored)
        if not stored:
            raise IndexIncompatibleError("index has no committed chunks")
        with open(output, "rb") as handle:
            for chunk in stored:
                offset = int(chunk["file_offset"])
                length = int(chunk["file_length"])
                handle.seek(offset)
                data = handle.read(length)
                if len(data) != length:
                    raise IndexIncompatibleError(
                        "chunk body is shorter than the index claims",
                        context={"chunk": chunk["chunk_index"], "offset": offset, "length": length},
                    )
                digest = hashlib.sha256(data).hexdigest()
                if digest != chunk["bytes_sha256"]:
                    self.summary.chunk_mismatches.append(int(chunk["chunk_index"]))
                self.summary.chunks_checked += 1
        if self.summary.chunk_mismatches:
            raise IndexIncompatibleError(
                "repaired output does not match the indexed chunk digests",
                context={"mismatched_chunks": self.summary.chunk_mismatches[:8]},
            )

