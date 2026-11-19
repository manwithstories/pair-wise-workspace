"""Pipeline orchestration: plan chunks from subtitle boundaries, analyze,
dedup, remux chunk-wise with checkpoints, and recover after interruption.

Output is appended to a temp PCM file with one SQLite transaction per chunk;
finalization writes the WAV to a temp file, validates it, then atomically
replaces the target. Recovery verifies committed chunks, rolls back the first
corrupt chunk and everything after it, and recomputes only those.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
from dataclasses import dataclass, asdict
from pathlib import Path

import numpy as np
import soundfile as sf

from . import analyze, probe, timestamps, timebase
from .errors import ErrorLog, MediaBatchError
from .index import CheckpointIndex, ChunkRecord, verify_chunks
from .subtitle import parse_subtitles

SAMPLE_RATE = timebase.DEFAULT_SAMPLE_RATE


class PipelineInterrupted(Exception):
    def __init__(self, committed: int):
        super().__init__(f"interrupted after {committed} committed chunk(s)")
        self.committed = committed


@dataclass
class ChunkPlan:
    seq: int
    start_ms: int
    end_ms: int
    snap_ms: int        # nearest video frame time to the end boundary
    deviation_ms: int   # snap_ms - end_ms (boundary landed between frames)
    text: str


def _nearest(values: list, target) -> object:
    return min(values, key=lambda v: abs(v - target)) if values else target


def build_plan(workdir: Path, log: ErrorLog) -> list[ChunkPlan]:
    inp = workdir / "input"
    cues = parse_subtitles(inp / "program.srt", log)
    if (inp / "program.vtt").exists():
        parse_subtitles(inp / "program.vtt", log)
    if (inp / "timecodes.txt").exists():
        codes = (inp / "timecodes.txt").read_text().split()
        timestamps.unwrap_timecodes(codes, timebase.FPS_2997, log,
                                    str(inp / "timecodes.txt"))
    video_times: list[int] = []
    mp4 = inp / "program.mp4"
    if mp4.exists():
        frames = probe.video_frame_times(mp4, log)
        video_times = [int(t * 1000) for t in frames]
    plan: list[ChunkPlan] = []
    for i, cue in enumerate(cues):
        snap = _nearest(video_times, cue.end_ms)
        plan.append(ChunkPlan(i, cue.start_ms, cue.end_ms,
                              int(snap), int(snap) - cue.end_ms, cue.text))
    return plan


def _save_json(path: Path, obj) -> None:
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_text(json.dumps(obj, ensure_ascii=False, indent=1), encoding="utf-8")
    os.replace(tmp, path)


def _load_json(path: Path, default):
    return json.loads(path.read_text(encoding="utf-8")) if path.exists() else default


def _write_pcm(path: Path, data: bytes, offset: int | None = None) -> None:
    try:
        with open(path, "r+b" if offset is not None else "ab") as fh:
            if offset is not None:
                fh.seek(offset)
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
    except OSError as exc:
        raise MediaBatchError("io_error", f"write failed on {path}: {exc}",
                              str(path), offset if offset is not None else -1)


def _finalize(workdir: Path, pcm_path: Path, total_samples: int) -> str:
    """Write the final WAV to a temp file, validate, atomically replace."""
    out_dir = workdir / "output"
    out_dir.mkdir(exist_ok=True)
    final = out_dir / "program.out.wav"
    tmp = out_dir / "program.out.wav.tmp"
    try:
        raw = pcm_path.read_bytes()[: total_samples * 2]
        samples = np.frombuffer(raw, dtype=np.int16).copy()
        sf.write(str(tmp), samples, SAMPLE_RATE, subtype="PCM_16", format="WAV")
        digest = hashlib.sha256(tmp.read_bytes()).hexdigest()
        os.replace(tmp, final)
    except OSError as exc:
        raise MediaBatchError("io_error", f"finalize failed: {exc}", str(final), 0)
    return digest


def run(workdir: str | Path, interrupt_after: int | None = None,
        resume: bool = False) -> dict:
    workdir = Path(workdir)
    work = workdir / "work"
    work.mkdir(parents=True, exist_ok=True)
    pcm_path = work / "output.pcm"
    fp_path = work / "fingerprints.json"
    plan_path = work / "plan.json"
    err_path = work / "errors.json"
    report: dict = {"recovery_seconds": 0.0, "reprocessed_chunks": []}

    log = ErrorLog.load(err_path)
    if resume and plan_path.exists():
        plan = [ChunkPlan(**c) for c in _load_json(plan_path, [])]
        index = CheckpointIndex(work / "index.db")
        t0 = time.monotonic()
        valid, corrupt = verify_chunks(index.committed_chunks(), pcm_path)
        if corrupt is not None:
            removed = index.rollback_from(corrupt.seq)
            pcm_path.write_bytes(pcm_path.read_bytes()[: corrupt.output_offset])
            report["reprocessed_chunks"] = [c.seq for c in
                                            index.committed_chunks()[len(valid):]]
            report["rolled_back"] = removed
        report["recovery_seconds"] = round(time.monotonic() - t0, 6)
        start_seq = len(valid)
        report["reprocessed_chunks"] = list(range(start_seq, len(plan)))
    else:
        if resume:
            raise MediaBatchError("no_checkpoint",
                                  "nothing to resume: no plan found", str(workdir), 0)
        for p in (work / "index.db", pcm_path, fp_path):
            p.unlink(missing_ok=True)
        log = ErrorLog()
        plan = build_plan(workdir, log)
        _save_json(plan_path, [asdict(c) for c in plan])
        index = CheckpointIndex(work / "index.db")
        index.set_meta("plan_chunks", str(len(plan)))
        start_seq = 0

    fingerprints: list[dict] = _load_json(fp_path, [])
    reps: list[tuple[int, str]] = [(f["seq"], f["fingerprint"])
                                   for f in fingerprints if f["duplicate_of"] is None]
    silence_ms: list[list[int]] = _load_json(work / "silence.json", [])
    committed = start_seq
    try:
        with sf.SoundFile(workdir / "input" / "program.wav") as snd:
            for chunk in plan[start_seq:]:
                s0 = timebase.ms_to_samples(chunk.start_ms, SAMPLE_RATE)
                s1 = timebase.ms_to_samples(chunk.end_ms, SAMPLE_RATE)
                snd.seek(s0)
                samples = snd.read(s1 - s0, dtype="int16", always_2d=False)
                if samples.ndim > 1:
                    samples = samples.mean(axis=1).astype(np.int16)
                raw = samples.tobytes()
                in_sha = hashlib.sha256(raw).hexdigest()
                fp = analyze.audio_fingerprint(samples, SAMPLE_RATE)
                duplicate_of = None
                for rseq, rfp in reps:
                    if rfp == fp or analyze.fingerprint_similarity(rfp, fp) >= 0.90:
                        duplicate_of = rseq
                        break
                for a, b in analyze.detect_silence(samples, SAMPLE_RATE):
                    silence_ms.append([chunk.start_ms + a, chunk.start_ms + b])
                offset = pcm_path.stat().st_size if pcm_path.exists() else 0
                payload = b"" if duplicate_of is not None else raw
                _write_pcm(pcm_path, payload)
                if duplicate_of is None:
                    reps.append((chunk.seq, fp))
                fingerprints.append({"seq": chunk.seq, "fingerprint": fp,
                                     "duplicate_of": duplicate_of})
                _save_json(fp_path, fingerprints)
                index.commit_chunk(ChunkRecord(
                    chunk.seq, in_sha, offset, len(payload),
                    hashlib.sha256(payload).hexdigest(), index.next_txn_id()))
                committed = chunk.seq + 1
                if interrupt_after is not None and committed >= interrupt_after:
                    raise PipelineInterrupted(committed)
    finally:
        log.save(err_path)
        _save_json(work / "silence.json", silence_ms)

    total = sum(timebase.ms_to_samples(c.end_ms - c.start_ms, SAMPLE_RATE)
                for c in plan
                if not any(f["seq"] == c.seq and f["duplicate_of"] is not None
                           for f in fingerprints))
    out_sha = _finalize(workdir, pcm_path, total)
    index.set_meta("output_sha256", out_sha)
    index.close()
    report.update({
        "chunks_committed": committed,
        "output_sha256": out_sha,
        "duplicates": {f["seq"]: f["duplicate_of"] for f in fingerprints
                       if f["duplicate_of"] is not None},
        "silence_ms": silence_ms,
        "subtitle_errors": [e.to_dict() for e in log
                            if e.source.endswith((".srt", ".vtt"))],
        "errors": [e.to_dict() for e in log],
    })
    _save_json(workdir / "output" / "manifest.json", report)
    return report
