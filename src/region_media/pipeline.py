"""End-to-end processing pipeline: scan -> measure -> normalize -> chunk.

Two passes keep memory bounded by the chunk/block size rather than the stream
length:

1. **measure** streams assembled source blocks through the *same* channel
   matrix and resampler the output will use, feeding a streaming BS.1770
   meter.  Measuring after the down/up-mix is what makes the reported LUFS
   describe the delivered file rather than the source.
2. **chunk** streams the blocks again, applies the gain chosen once for the
   whole run, and commits complete chunks atomically.  A resumed run replays
   the audio to keep the resampler state exact but skips encoding chunks that
   are already committed and hash-verified.
"""

from __future__ import annotations

import math
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np

from . import (
    assemble,
    checkpoint,
    codec,
    manifest as manifest_mod,
    metadata,
    normalize,
    packet,
)
from .errors import (
    Code,
    Diagnostic,
    DiagnosticCollector,
    InputError,
    Interrupted,
    ParamError,
)

DEFAULT_CHUNK_MS = 1000
SUPPORTED_RATES = (44100, 48000, 96000)
SUPPORTED_LAYOUTS = ("mono", "stereo", "5.1", "7.1")


@dataclass
class ProcessOptions:
    input_dir: Path
    out_dir: Path
    rate: int = 48000
    layout: str = "stereo"
    lufs: float = -16.0
    chunk_ms: int = DEFAULT_CHUNK_MS
    target_kbps: float = 256.0
    peak_dbfs: float = normalize.PEAK_CEILING_DBFS
    simulate_interrupt_after: int | None = None
    resume: bool = False
    block_ms: int = 100

    def validate(self) -> None:
        if self.rate not in SUPPORTED_RATES:
            raise ParamError(
                Code.UNSUPPORTED_LAYOUT,
                f"--rate must be one of {SUPPORTED_RATES}",
                detail={"rate": self.rate},
            )
        canonical = normalize.canonical_layout(self.layout)
        if canonical not in SUPPORTED_LAYOUTS:
            raise ParamError(
                Code.UNSUPPORTED_LAYOUT,
                f"--layout must be one of {SUPPORTED_LAYOUTS}",
                detail={"layout": self.layout},
            )
        self.layout = canonical
        if not -40.0 <= self.lufs <= -5.0:
            raise ParamError(
                Code.UNSUPPORTED_LAYOUT,
                "--lufs must be between -40 and -5",
                detail={"lufs": self.lufs},
            )
        if not 20 <= self.chunk_ms <= 60000:
            raise ParamError(
                Code.UNSUPPORTED_LAYOUT,
                "--chunk-ms must be between 20 and 60000",
                detail={"chunk_ms": self.chunk_ms},
            )
        if not 8.0 <= self.target_kbps <= 2048.0:
            raise ParamError(
                Code.UNSUPPORTED_LAYOUT,
                "--target-kbps must be between 8 and 2048",
                detail={"target_kbps": self.target_kbps},
            )
        if not -20.0 <= self.peak_dbfs <= 0.0:
            raise ParamError(
                Code.UNSUPPORTED_LAYOUT,
                "--peak-dbfs must be between -20 and 0",
                detail={"peak_dbfs": self.peak_dbfs},
            )
        if not self.input_dir.is_dir():
            raise ParamError(
                Code.MALFORMED_HEADER,
                f"input directory not found: {self.input_dir}",
                detail={"input": str(self.input_dir)},
            )
        if (
            self.simulate_interrupt_after is not None
            and self.simulate_interrupt_after < 1
        ):
            raise ParamError(
                Code.UNSUPPORTED_LAYOUT,
                "--simulate-interrupt-after must be at least 1",
            )

    def parameters(self) -> dict[str, Any]:
        return {
            "rate": self.rate,
            "layout": self.layout,
            "lufs": self.lufs,
            "chunk_ms": self.chunk_ms,
            "target_kbps": self.target_kbps,
            "peak_dbfs": self.peak_dbfs,
        }


@dataclass
class ProcessResult:
    exit_code: int
    summary: dict[str, Any]
    manifest_path: Path | None = None
    diagnostics: list[Diagnostic] = field(default_factory=list)

    def to_json(self) -> dict[str, Any]:
        return {"exit_code": self.exit_code, **self.summary}


@dataclass
class VerifyResult:
    exit_code: int
    summary: dict[str, Any]

    def to_json(self) -> dict[str, Any]:
        return {"exit_code": self.exit_code, **self.summary}


# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------


def _utc_now() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def _block_samples(rate: int, ms: int) -> int:
    return max(1, int(round(rate * ms / 1000.0)))


def _clear_stale_run(out_dir: Path) -> None:
    """Remove artifacts of a previous, non-resumed run in the same directory."""
    for name in (checkpoint.INDEX_NAME, checkpoint.CHECKPOINT_NAME,
                 manifest_mod.MANIFEST_NAME):
        (out_dir / name).unlink(missing_ok=True)
    for path in out_dir.glob("chunk-*"):
        if path.is_file():
            path.unlink(missing_ok=True)
    quarantine = out_dir / checkpoint.QUARANTINE_DIR
    if quarantine.is_dir():
        for path in quarantine.iterdir():
            if path.is_file():
                path.unlink(missing_ok=True)
        quarantine.rmdir()


def _measure_pass(
    options: ProcessOptions, spec: packet.StreamSpec, block_samples: int
) -> tuple[float | None, float | None, int]:
    """Stream mixed+resampled audio into a BS.1770 meter; no audio is kept."""
    meter = normalize.LoudnessMeter(options.rate)
    normalizer = normalize.ChunkNormalizer(
        source_rate=spec.sample_rate,
        target_rate=options.rate,
        source_layout=spec.layout,
        target_layout=options.layout,
    )
    normalizer.prepare()
    for block in assemble.iter_source_blocks(
        options.input_dir, block_samples=block_samples
    ):
        if block.samples.size == 0:
            continue
        mixed = normalizer.matrix.apply(block.samples.astype(np.float32))
        if not np.isfinite(mixed).all():
            normalizer.nonfinite_samples += int((~np.isfinite(mixed)).sum())
            mixed = np.nan_to_num(mixed, nan=0.0, posinf=0.0, neginf=0.0)
        resampled = normalizer.resample(mixed)
        if resampled.size:
            meter.push(resampled)
    tail = normalizer.flush()
    if tail.size:
        meter.push(tail)
    lufs, peak = meter.result()
    return lufs, peak, normalizer.nonfinite_samples


@dataclass
class ReadyChunk:
    """A filled chunk plus the source packet range that produced it."""

    samples: np.ndarray
    out_start: int          # first output sample of this chunk
    out_end: int            # one past the last output sample
    source_start: int       # first source sample (timeline position)
    source_end: int         # one past the last source sample
    sequence_start: int | None
    sequence_end: int | None
    timestamp_start: int | None
    timestamp_end: int | None
    timestamp_wrapped: bool


@dataclass
class _BlockSpan:
    source_start: int
    source_end: int
    sequences: tuple[int, ...]
    timestamps: tuple[int, ...]
    timestamp_wrapped: bool


class ChunkAccumulator:
    """Collects processed samples into fixed-size chunks with source spans.

    Block spans are recorded in *source* sample coordinates.  Because the
    assembler emits contiguous blocks and the matrix/resampler only scale time,
    a chunk's output range maps to a source range by a single ratio.
    """

    def __init__(self, chunk_samples: int, channels: int, ratio: float) -> None:
        if chunk_samples <= 0:
            raise ValueError("chunk_samples must be positive")
        self.chunk_samples = chunk_samples
        self.channels = channels
        self.ratio = ratio  # source samples per output sample
        self._parts: list[np.ndarray] = []
        self._filled = 0
        self._spans: list[_BlockSpan] = []
        self._produced = 0

    def add_block(self, block: assemble.SourceBlock) -> None:
        if block.samples.shape[0] == 0:
            return
        self._spans.append(
            _BlockSpan(
                source_start=block.start,
                source_end=block.start + int(block.samples.shape[0]),
                sequences=tuple(block.sequences),
                timestamps=tuple(block.timestamps),
                timestamp_wrapped=block.timestamp_wrapped,
            )
        )

    def _span_for(
        self, out_start: int, out_end: int
    ) -> tuple[int, int, int | None, int | None, int, int, bool]:
        src_start = int(round(out_start * self.ratio))
        src_end = int(round(out_end * self.ratio))
        seq_first = seq_last = ts_first = ts_last = None
        wrapped = False
        for span in self._spans:
            if span.source_end <= src_start:
                continue
            if span.source_start >= src_end:
                break
            if span.sequences:
                if seq_first is None:
                    seq_first = span.sequences[0]
                    ts_first = span.timestamps[0] if span.timestamps else None
                seq_last = span.sequences[-1]
                ts_last = span.timestamps[-1] if span.timestamps else None
            wrapped = wrapped or span.timestamp_wrapped
        return seq_first, seq_last, ts_first, ts_last, src_start, src_end, wrapped

    def _make_ready(self, audio: np.ndarray) -> ReadyChunk:
        out_start = self._produced
        out_end = out_start + int(audio.shape[0])
        seqs = self._span_for(out_start, out_end)
        self._produced = out_end
        # Drop spans that can no longer be referenced by any future chunk.
        self._spans = [sp for sp in self._spans if sp.source_end > seqs[4]]
        return ReadyChunk(
            samples=audio,
            out_start=out_start,
            out_end=out_end,
            source_start=seqs[4],
            source_end=seqs[5],
            sequence_start=seqs[0],
            sequence_end=seqs[1],
            timestamp_start=seqs[2],
            timestamp_end=seqs[3],
            timestamp_wrapped=seqs[6],
        )

    def push(self, samples: np.ndarray) -> list[ReadyChunk]:
        ready: list[ReadyChunk] = []
        offset = 0
        total = samples.shape[0]
        while offset < total:
            take = min(self.chunk_samples - self._filled, total - offset)
            self._parts.append(samples[offset : offset + take])
            self._filled += take
            offset += take
            if self._filled == self.chunk_samples:
                ready.append(self._make_ready(np.concatenate(self._parts, axis=0)))
                self._parts = []
                self._filled = 0
        return ready

    def flush(self) -> ReadyChunk | None:
        if not self._parts:
            return None
        audio = np.concatenate(self._parts, axis=0)
        self._parts = []
        self._filled = 0
        return self._make_ready(audio)


# ---------------------------------------------------------------------------
# process
# ---------------------------------------------------------------------------


def _chunk_media_samples(path: Path, record: checkpoint.ChunkRecord) -> int:
    """Decoded media length of a committed chunk, trimming codec padding."""
    decoded, _rate, _layout = codec.probe_chunk(path)
    if decoded >= record.samples:
        return record.samples
    return decoded


def process(options: ProcessOptions) -> ProcessResult:
    """Run the full pipeline; returns an exit code plus JSON summary."""
    options.validate()
    collector = DiagnosticCollector()
    out_dir = options.out_dir
    out_dir.mkdir(parents=True, exist_ok=True)
    checkpoint_path = out_dir / checkpoint.CHECKPOINT_NAME
    manifest_path = out_dir / manifest_mod.MANIFEST_NAME
    index_path = out_dir / checkpoint.INDEX_NAME

    # ---- resume preconditions --------------------------------------------
    state: checkpoint.CheckpointState | None = None
    if options.resume:
        if not checkpoint_path.is_file() or not index_path.is_file():
            raise InputError(
                Code.RESUME_UNAVAILABLE,
                f"no checkpoint to resume in {out_dir}",
                where=str(checkpoint_path),
            )
        state = checkpoint.CheckpointState.load(checkpoint_path)
        probe = assemble.scan_capture(options.input_dir)
        if state.fingerprint and state.fingerprint != probe.fingerprint:
            raise InputError(
                Code.INPUT_CHANGED,
                "capture changed since the checkpoint was written",
                where=str(checkpoint_path),
                detail={
                    "expected": state.fingerprint,
                    "actual": probe.fingerprint,
                },
            )
        if state.parameters and state.parameters != options.parameters():
            raise InputError(
                Code.INPUT_CHANGED,
                "processing parameters differ from the checkpoint",
                where=str(checkpoint_path),
                detail={
                    "checkpoint": state.parameters,
                    "requested": options.parameters(),
                },
            )
    else:
        _clear_stale_run(out_dir)

    # ---- scan & metadata --------------------------------------------------
    analysis = assemble.scan_capture(options.input_dir)
    collector.extend(analysis.diagnostics)
    video_report, video_diags = metadata.inspect_video_metadata(
        analysis.videos, where=str(options.input_dir)
    )
    collector.extend(video_diags)
    # The pipeline preserves video records byte-for-byte; this guard makes
    # that invariant checkable rather than assumed.
    collector.extend(
        metadata.timeline_shift_diagnostics(
            video_report,
            source_records=analysis.videos,
            output_records=metadata.video_output_records(analysis.videos),
            clock_rate=analysis.spec.clock_rate,
        )
    )
    spec = analysis.spec

    # ---- unsupported source layout ---------------------------------------
    if normalize.canonical_layout(spec.layout) is None:
        collector.add_code(
            Code.UNSUPPORTED_LAYOUT,
            f"source layout {spec.layout!r} is not in the conversion matrix",
            where=str(options.input_dir),
            detail={"layout": spec.layout, "channels": spec.channels},
        )
        _write_manifest(
            manifest_path,
            options,
            analysis,
            [],
            collector,
            video_report,
            resumed=options.resume,
            reused=0,
            stopped=False,
            quarantined=[],
        )
        return ProcessResult(
            exit_code=1,
            summary=_summary(
                manifest_path, collector, [], 0, stopped=False, analysis=analysis
            ),
            manifest_path=manifest_path,
            diagnostics=collector.samples,
        )

    # ---- measure ----------------------------------------------------------
    block_samples = _block_samples(options.rate, options.block_ms)
    source_lufs, source_peak, nonfinite = _measure_pass(options, spec, block_samples)
    if nonfinite:
        collector.add_code(
            Code.NONFINITE_SAMPLE,
            f"{nonfinite} non-finite samples replaced with zero",
            where=str(options.input_dir),
        )
    gain_db, peak_limited = normalize.compute_gain(
        source_lufs,
        source_peak,
        target_lufs=options.lufs,
        peak_ceiling_dbfs=options.peak_dbfs,
    )

    index = checkpoint.ChunkIndex(index_path)
    quarantined: list[dict[str, Any]] = []
    final_records: list[checkpoint.ChunkRecord] = []
    reused_count = 0
    stopped = False
    try:
        # ---- verify anything a previous run committed ---------------------
        reused: dict[int, checkpoint.ChunkRecord] = {}
        if options.resume:
            outcome = checkpoint.verify_committed(
                out_dir,
                index,
                decode=lambda path, record: _chunk_media_samples(path, record),
            )
            collector.extend(outcome.diagnostics)
            quarantined = outcome.quarantined
            for record in index.records():
                if record.index in set(outcome.verified):
                    reused[record.index] = record
            state = checkpoint.CheckpointState(
                fingerprint=analysis.fingerprint,
                input_dir=str(options.input_dir),
                parameters=options.parameters(),
            )
        else:
            state = checkpoint.CheckpointState(
                fingerprint=analysis.fingerprint,
                input_dir=str(options.input_dir),
                parameters=options.parameters(),
            )
        index.set_meta("fingerprint", analysis.fingerprint)
        index.set_meta("parameters", options.parameters())
        index.set_meta("video", [v.to_json() for v in analysis.videos])
        index.set_meta("source", analysis.summary())

        # ---- second pass: apply gain, chunk and commit --------------------
        normalizer = normalize.ChunkNormalizer(
            source_rate=spec.sample_rate,
            target_rate=options.rate,
            source_layout=spec.layout,
            target_layout=options.layout,
            target_lufs=options.lufs,
            peak_ceiling_dbfs=options.peak_dbfs,
            gain_db=gain_db,
        )
        normalizer.prepare()
        normalizer.peak_limited = peak_limited
        chunk_samples = max(1, int(round(options.rate * options.chunk_ms / 1000.0)))
        channels = len(normalize.CANONICAL_LAYOUTS[options.layout])
        accumulator = ChunkAccumulator(
            chunk_samples, channels, spec.sample_rate / options.rate
        )
        extension = codec.choose_codec(options.rate).extension
        bit_rate = int(round(options.target_kbps * 1000))
        written_chunks: list[checkpoint.ChunkRecord] = []

        next_number = 0

        def emit(ready: ReadyChunk) -> None:
            nonlocal reused_count, written_chunks, next_number
            number = next_number
            next_number += 1
            audio = ready.samples
            short = audio.shape[0] < chunk_samples
            clipped = normalizer.take_clipped()
            prior = reused.get(number)
            if prior is not None:
                reused_count += 1
                return
            file_name = f"chunk-{number:05d}{extension}"
            local_lufs = normalize.integrated_loudness(audio, options.rate)
            local_peak = normalize.true_peak_dbfs(audio)
            info = codec.encode_chunk(
                out_dir / file_name,
                audio,
                options.rate,
                options.layout,
                bit_rate=bit_rate,
            )
            digest = checkpoint.sha256_file(out_dir / file_name)
            errors: list[str] = []
            if short:
                errors.append(Code.SHORT_CHUNK.value)
            if local_lufs is None:
                errors.append(Code.SILENT_BLOCK.value)
            record = checkpoint.ChunkRecord(
                index=number,
                name=file_name,
                sha256=digest,
                size_bytes=info["size_bytes"],
                samples=int(audio.shape[0]),
                start_sample=ready.out_start,
                end_sample=ready.out_end,
                source_start=ready.source_start,
                source_end=ready.source_end,
                actual_kbps=info["size_bytes"]
                * 8.0
                / (audio.shape[0] / options.rate)
                / 1000.0,
                peak_dbfs=None if local_peak is None else round(local_peak, 4),
                loudness=None if local_lufs is None else round(local_lufs, 4),
                clipped_samples=clipped,
                silent=local_lufs is None,
                short=short,
                errors=tuple(errors),
                chunk_id=f"chunk-{number:05d}",
                start_seconds=ready.out_start / options.rate,
                end_seconds=ready.out_end / options.rate,
                source_sequence_start=ready.sequence_start,
                source_sequence_end=ready.sequence_end,
                timestamp_start=ready.timestamp_start,
                timestamp_end=ready.timestamp_end,
                timestamp_wrapped=ready.timestamp_wrapped,
                out_rate=options.rate,
            )
            index.add(record, committed_at=_utc_now())
            written_chunks.append(record)
            state.committed = len(reused) + len(written_chunks)
            state.total_chunks = state.committed
            state.quarantined = len(quarantined)
            state.updated_at = _utc_now()
            checkpoint.atomic_write_json(checkpoint_path, state.to_json())
            if (
                options.simulate_interrupt_after is not None
                and len(written_chunks) >= options.simulate_interrupt_after
            ):
                raise Interrupted(
                    Code.INPUT_CHANGED,
                    f"simulated interruption after {len(written_chunks)} new chunks",
                    detail={
                        "committed": state.committed,
                        "next_chunk": number + 1,
                        "reused": len(reused),
                    },
                )

        for block in assemble.iter_source_blocks(
            options.input_dir, block_samples=block_samples
        ):
            accumulator.add_block(block)
            processed = normalizer.process(block.samples)
            for ready in accumulator.push(processed):
                emit(ready)
        tail = normalizer.flush()
        if tail.size:
            for ready in accumulator.push(tail):
                emit(ready)
        remainder = accumulator.flush()
        if remainder is not None:
            emit(remainder)

        final_records = sorted(
            list(reused.values()) + written_chunks, key=lambda r: r.index
        )
        state.complete = True
        state.committed = len(final_records)
        state.total_chunks = len(final_records)
        state.quarantined = len(quarantined)
        state.updated_at = _utc_now()
        checkpoint.atomic_write_json(checkpoint_path, state.to_json())
    finally:
        index.close()

    _write_manifest(
        manifest_path,
        options,
        analysis,
        final_records,
        collector,
        video_report,
        resumed=options.resume,
        reused=reused_count,
        stopped=stopped,
        quarantined=quarantined,
        source_lufs=source_lufs,
        source_peak=source_peak,
        gain_db=gain_db,
        peak_limited=peak_limited,
    )
    summary = _summary(
        manifest_path,
        collector,
        final_records,
        reused_count,
        stopped=False,
        analysis=analysis,
    )
    summary.update(
        {
            "gain_db": round(gain_db, 3),
            "peak_limited": peak_limited,
            "source_lufs": None if source_lufs is None else round(source_lufs, 3),
            "source_true_peak_dbfs": None
            if source_peak is None
            else round(source_peak, 3),
            "quarantined_chunks": len(quarantined),
            "resumed": options.resume,
        }
    )
    return ProcessResult(
        exit_code=0,
        summary=summary,
        manifest_path=manifest_path,
        diagnostics=collector.samples,
    )


# ---------------------------------------------------------------------------
# manifest assembly
# ---------------------------------------------------------------------------


def _manifest_chunks(
    records: list[checkpoint.ChunkRecord], options: ProcessOptions, src_rate: int
) -> list[manifest_mod.ChunkManifest]:
    entries = []
    for record in sorted(records, key=lambda r: r.index):
        entries.append(
            manifest_mod.ChunkManifest(
                record=record,
                source_rate=src_rate,
                target_rate=options.rate,
                start_seconds=record.start_sample / options.rate,
                end_seconds=record.end_sample / options.rate,
                source_start_seconds=record.source_start / src_rate,
                source_end_seconds=record.source_end / src_rate,
            )
        )
    return entries


def _matrix_json(source_layout: str, target_layout: str) -> dict[str, Any] | None:
    try:
        matrix = normalize.channel_matrix(source_layout, target_layout)
    except ValueError:
        return None
    return {
        "source": matrix.source,
        "target": matrix.target,
        "source_channels": list(normalize.CANONICAL_LAYOUTS[matrix.source]),
        "target_channels": list(normalize.CANONICAL_LAYOUTS[matrix.target]),
        "weights": matrix.weights.round(6).tolist(),
    }


def _write_manifest(
    path: Path,
    options: ProcessOptions,
    analysis: assemble.CaptureAnalysis,
    records: list[checkpoint.ChunkRecord],
    collector: DiagnosticCollector,
    video_report: metadata.VideoMetadataReport,
    *,
    resumed: bool,
    reused: int,
    stopped: bool,
    quarantined: list[dict[str, Any]],
    source_lufs: float | None = None,
    source_peak: float | None = None,
    gain_db: float = 0.0,
    peak_limited: bool = False,
) -> None:
    spec = analysis.spec
    entries = _manifest_chunks(records, options, spec.sample_rate)
    video_start = min((v.pts_samples for v in analysis.videos), default=None)
    video_end = max(
        (v.pts_samples + v.duration_samples for v in analysis.videos), default=None
    )
    video_json = video_report.to_json()
    video_json.update(
        {
            "start_samples": video_start,
            "end_samples": video_end,
            "start_seconds": None
            if video_start is None
            else video_start / spec.clock_rate,
            "end_seconds": None if video_end is None else video_end / spec.clock_rate,
            "preserved_records": metadata.video_output_records(analysis.videos),
        }
    )
    source_json = analysis.summary()
    source_json.update(
        {
            "lost_packet_count": analysis.lost_packets,
            "lost_sequences": list(analysis.lost_sequences),
            "source_lufs": None if source_lufs is None else round(source_lufs, 4),
            "source_true_peak_dbfs": None
            if source_peak is None
            else round(source_peak, 4),
            "gain_db": round(gain_db, 4),
            "peak_limited": peak_limited,
            "channel_matrix": _matrix_json(spec.layout, options.layout),
        }
    )
    codes = collector.to_json()["codes"]
    manifest = manifest_mod.Manifest(
        input_dir=str(options.input_dir),
        fingerprint=analysis.fingerprint,
        parameters=options.parameters(),
        source=source_json,
        video=video_json,
        chunks=entries,
        errors=codes,
        diagnostics=collector.to_json()["samples"],
        intervals=manifest_mod.build_intervals(entries),
        stopped_early=stopped,
        resumed=resumed,
        reused_chunks=reused,
        quarantined_chunks=quarantined,
    )
    manifest.write(path)


def _summary(
    manifest_path: Path,
    collector: DiagnosticCollector,
    records: list[checkpoint.ChunkRecord],
    reused: int,
    *,
    stopped: bool,
    analysis: assemble.CaptureAnalysis | None = None,
) -> dict[str, Any]:
    codes = collector.to_json()["codes"]
    def count(code: Code, explicit: int | None = None) -> int:
        if explicit is not None:
            return explicit
        return int(codes.get(code.value, 0))
    return {
        "manifest": str(manifest_path),
        "chunks": len(records),
        "samples": sum(r.samples for r in records),
        "reused_chunks": reused,
        "stopped_early": stopped,
        "errors": codes,
        "lost_packets": count(
            Code.LOST_PACKET,
            analysis.lost_packets if analysis is not None else None,
        ),
        "reordered_packets": count(
            Code.REORDERED_PACKET,
            analysis.reordered_packets if analysis is not None else None,
        ),
        "duplicate_packets": count(
            Code.DUPLICATE_PACKET,
            analysis.duplicate_packets if analysis is not None else None,
        ),
        "truncated_packets": count(
            Code.TRUNCATED_PACKET,
            analysis.truncated_packets if analysis is not None else None,
        ),
        "truncated_records": count(Code.TRUNCATED_RECORD,
                                   analysis.record_errors if analysis else None),
        "metadata_conflicts": int(codes.get(Code.METADATA_CONFLICT.value, 0)),
    }


# ---------------------------------------------------------------------------
# verify
# ---------------------------------------------------------------------------


def verify(out_dir: Path, manifest_path: Path) -> VerifyResult:
    """Recompute quality and alignment metrics from files + manifest."""
    out_dir = Path(out_dir)
    manifest_path = Path(manifest_path)
    if not manifest_path.is_file():
        raise InputError(
            Code.MALFORMED_HEADER,
            f"manifest not found: {manifest_path}",
            where=str(manifest_path),
        )
    try:
        data = manifest_mod.load_manifest(manifest_path)
    except (OSError, ValueError) as exc:
        raise InputError(
            Code.MALFORMED_HEADER, str(exc), where=str(manifest_path)
        ) from exc

    parameters = data.get("parameters", {})
    rate = int(parameters.get("rate", 0))
    if rate <= 0:
        raise InputError(
            Code.MALFORMED_HEADER,
            "manifest has no target rate",
            where=str(manifest_path),
        )
    layout = str(parameters.get("layout", "stereo"))
    target_kbps = float(parameters.get("target_kbps", 0.0))

    total_bytes = 0
    total_samples = 0
    decoded_cursor = 0
    worst_alignment_ms = 0.0
    worst_interval_ms = 0.0
    missing: list[int] = []
    hash_mismatch: list[int] = []
    decode_errors: list[dict[str, Any]] = []
    lufs_values: list[float] = []
    peak_values: list[float] = []
    clipped = 0
    previous_end: float | None = None
    source_cursor_samples = 0
    source_rate = int(data.get("source", {}).get("source_rate", rate) or rate)
    expected_source_start = 0.0
    entries = data.get("chunks", [])
    for entry in entries:
        number = int(entry.get("index", -1))
        path = out_dir / str(entry.get("name", ""))
        if not path.is_file():
            missing.append(number)
            continue
        if checkpoint.sha256_file(path) != entry.get("sha256"):
            hash_mismatch.append(number)
            continue
        total_bytes += path.stat().st_size
        try:
            decoded = codec.decode_chunk(
                path,
                expected_samples=int(entry.get("samples", 0)),
                expected_layout=layout,
            )
        except codec.CodecError as exc:
            decode_errors.append({"index": number, "error": exc.message})
            continue
        total_samples += decoded.samples_out
        start_manifest = float(entry.get("start_seconds", 0.0))
        start_decoded = decoded_cursor / rate
        worst_alignment_ms = max(
            worst_alignment_ms, abs(start_decoded - start_manifest) * 1000.0
        )
        if previous_end is not None:
            worst_interval_ms = max(
                worst_interval_ms, abs(start_manifest - previous_end) * 1000.0
            )
        previous_end = float(entry.get("end_seconds", 0.0))
        # Compare the manifest's media start against a fresh running sum of
        # decoded samples: this is the error that would actually shift a chunk
        # on the timeline.
        source_start_seconds = float(entry.get("source_start_seconds", 0.0))
        expected_source_start = source_cursor_samples / source_rate
        worst_alignment_ms = max(
            worst_alignment_ms,
            abs(expected_source_start - source_start_seconds) * 1000.0,
        )
        source_cursor_samples += int(
            round(decoded.samples_out * source_rate / rate)
        )
        decoded_cursor += decoded.samples_out
        if entry.get("loudness") is not None:
            lufs_values.append(float(entry["loudness"]))
        if entry.get("peak_dbfs") is not None:
            peak_values.append(float(entry["peak_dbfs"]))
        clipped += int(entry.get("clipped_samples", 0) or 0)

    media_seconds = total_samples / rate if rate else 0.0
    actual_kbps = (
        total_bytes * 8.0 / media_seconds / 1000.0 if media_seconds > 0 else 0.0
    )
    deviation = manifest_mod.bitrate_deviation_pct(actual_kbps, target_kbps)
    integrated = (
        None
        if not lufs_values
        else 10.0
        * math.log10(sum(10.0 ** (v / 10.0) for v in lufs_values) / len(lufs_values))
    )
    true_peak = max(peak_values) if peak_values else None
    manifest_alignment = manifest_mod.manifest_alignment_ms(data)
    worst_alignment_ms = max(worst_alignment_ms, manifest_alignment)

    # Bitrate is only a meaningful acceptance criterion once there is enough
    # media for container overhead to amortise.  Empty and very short streams
    # (single packet, sub-250 ms) are reported as defined but not applicable.
    bitrate_applicable = media_seconds >= 0.25
    checks = {
        "bitrate_within_tolerance": (not bitrate_applicable)
        or abs(deviation) <= 10.0,
        "alignment_within_tolerance": worst_alignment_ms <= 2.0,
        "all_chunks_present": not missing and not hash_mismatch and not decode_errors,
        "finite_quality_metrics": _finite_or_none(integrated)
        and _finite_or_none(true_peak)
        and math.isfinite(float(deviation))
        and clipped >= 0,
    }
    ok = all(checks.values())
    codes = data.get("errors", {})
    source_info = data.get("source", {})
    lost_packets = int(source_info.get("lost_packet_count", 0)) or int(
        codes.get(Code.LOST_PACKET.value, 0)
    )
    summary = {
        "manifest": str(manifest_path),
        "chunks": len(entries),
        "samples": total_samples,
        "media_seconds": round(media_seconds, 6),
        "bitrate_applicable": bitrate_applicable,
        "target_kbps": target_kbps,
        "actual_kbps": round(actual_kbps, 4),
        "bitrate_deviation_pct": round(deviation, 4),
        "integrated_lufs": None if integrated is None else round(integrated, 4),
        "true_peak_dbfs": None if true_peak is None else round(true_peak, 4),
        "clipped_samples": clipped,
        "lost_packets": lost_packets,
        "reordered_packets": int(
            source_info.get("reordered_packets",
                            codes.get(Code.REORDERED_PACKET.value, 0))
        ),
        "duplicate_packets": int(
            source_info.get("duplicate_packets",
                            codes.get(Code.DUPLICATE_PACKET.value, 0))
        ),
        "truncated_packets": int(
            source_info.get("truncated_packets",
                            codes.get(Code.TRUNCATED_PACKET.value, 0))
        ),
        "truncated_records": int(
            source_info.get("record_errors",
                            codes.get(Code.TRUNCATED_RECORD.value, 0))
        ),
        "nonfinite_samples": int(
            source_info.get("nonfinite_samples",
                            codes.get(Code.NONFINITE_SAMPLE.value, 0))
        ),
        "metadata_conflicts": int(codes.get(Code.METADATA_CONFLICT.value, 0)),
        "metadata_missing": int(codes.get(Code.METADATA_MISSING.value, 0)),
        "manifest_alignment_ms": round(worst_alignment_ms, 6),
        "interval_gap_ms": round(worst_interval_ms, 6),
        "missing_chunks": missing,
        "hash_mismatches": hash_mismatch,
        "decode_errors": decode_errors,
        "checks": checks,
        "passed": ok,
    }
    return VerifyResult(exit_code=0 if ok else 3, summary=summary)


def _finite_or_none(value: float | None) -> bool:
    """Quality metrics must be finite whenever the stream carries audio.

    A stream with real samples but no measurable loudness/peak (e.g. all
    zeros) still yields a defined ``None``; only non-finite numbers fail.
    """
    return value is None or math.isfinite(value)


__all__ = [
    "ProcessOptions",
    "ProcessResult",
    "VerifyResult",
    "process",
    "verify",
]
