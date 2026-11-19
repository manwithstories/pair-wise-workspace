"""Pipeline orchestration: plan chunks from subtitle/timecode boundaries,
analyze, dedup, remux chunk-wise with checkpoints, and recover after
interruption.

Output is appended to a temp PCM file with one SQLite transaction per chunk;
finalization writes the WAV in bounded blocks, validates it, then atomically
replaces the target. Recovery verifies committed chunks against the PCM file,
rolls back the first corrupt chunk and everything after it, and recomputes
only those.
"""

from __future__ import annotations

import hashlib
import json
import os
import time
from dataclasses import dataclass, asdict
from fractions import Fraction
from pathlib import Path

import soundfile as sf

from . import analyze, probe, timestamps, timebase
from .errors import ErrorLog, MediaBatchError
from .index import CheckpointIndex, ChunkRecord, verify_chunks
from .output import BLOCK_BYTES, finalize_wav, sha256_streaming
from .subtitle import parse_subtitles

SAMPLE_RATE = timebase.DEFAULT_SAMPLE_RATE
DUP_THRESHOLD = 0.95


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
    group: int = -1     # source cue ordinal; pieces of one cue never dedup
                        # against each other because they carry different audio


def _nearest(values: list, target) -> object:
    return min(values, key=lambda v: abs(v - target)) if values else target


def _timecode_cut_ms(workdir: Path, log: ErrorLog) -> list[int]:
    """Convert in-program timecode files into cut points in milliseconds.

    Each file may start with a ``# fps=`` header; timecodes beyond the source
    duration are dropped rather than wrapping into the middle of the programme.
    """
    cuts: list[int] = []
    for name in ("timecodes_25.txt", "timecodes_24.txt", "timecodes_30.txt"):
        path = workdir / "input" / name
        if not path.exists():
            continue
        data = path.read_text(encoding="utf-8").splitlines()
        fps = Fraction(25)
        for line in data:
            line = line.strip()
            if not line:
                continue
            if line.startswith("#") and "fps=" in line:
                fps = timebase.parse_frame_rate(line.split("fps=", 1)[1].strip())
                continue
            try:
                frames = timestamps.timecode_to_frames(line, fps)
            except ValueError as exc:
                log.add("invalid_timecode", str(exc), str(path),
                        _line_offset(path, line), len(cuts))
                continue
            cuts.append(int(timebase.frames_to_samples(frames, fps, 1000)))
    return sorted(set(cuts))


def _line_offset(path: Path, needle: str) -> int:
    """Byte offset of the first line equal to `needle` (for error provenance)."""
    pos = 0
    for raw in path.read_bytes().split(b"\n"):
        if raw.rstrip(b"\r").strip().decode("utf-8", "replace") == needle:
            return pos
        pos += len(raw) + 1
    return -1


def _split_at_cuts(cues, cuts: list[int],
                   log: ErrorLog) -> list[tuple[int, int, str, int]]:
    """Split cue windows at timecode cut points.

    A cut inside a cue splits it into pieces that together cover exactly the
    original span; cuts outside every cue are ignored (they would either
    duplicate cue audio or extend past the programme). The fourth element is
    the source cue ordinal, used to keep pieces of one cue from being
    deduplicated against each other.
    """
    out: list[tuple[int, int, str, int, int]] = []
    for ordinal, cue in enumerate(cues):
        points = [c for c in cuts if cue.start_ms < c < cue.end_ms]
        start = cue.start_ms
        for piece, cut in enumerate(points + [cue.end_ms]):
            label = cue.text if piece == 0 else f"{cue.text} (cut part {piece})"
            out.append((start, cut, label, ordinal, cue.source_offset))
            start = cut
    return out


def build_plan(workdir: Path, log: ErrorLog,
               drift_reports: list | None = None) -> list[ChunkPlan]:
    inp = workdir / "input"
    source = inp / "program.wav"
    if not source.exists():
        log.add("missing_audio_track",
                f"programme audio {source} is missing; cannot plan chunks",
                str(source), 0, -1)
        raise MediaBatchError("missing_audio_track",
                              f"programme audio not found: {source}",
                              str(source), 0)
    cues = parse_subtitles(inp / "program.srt", log)
    if (inp / "program.vtt").exists():
        parse_subtitles(inp / "program.vtt", log)
    reports = drift_reports if drift_reports is not None else []
    for media in ("program.mp4", "drift.mka"):
        path = inp / media
        if path.exists():
            reports.append((str(path), measure_drift(path, log)))
    codes = inp / "timecodes.txt"
    if codes.exists():
        pairs = timestamps.split_timecode_lines(codes.read_bytes())
        fps = Fraction(30_000, 1001)
        lines = [c for c, _ in pairs if not c.startswith("#")]
        offs = [o for c, o in pairs if not c.startswith("#")]
        timestamps.unwrap_timecodes(lines, fps, log, str(codes), offs)

    video_times: list[int] = []
    mp4 = inp / "program.mp4"
    if mp4.exists():
        frames = probe.video_frame_times(mp4, log)
        video_times = [int(f * 1000) for f in frames]
    for name in ("truncated.mp4", "video_only.mp4", "broken.mp4"):
        path = inp / name
        if not path.exists():
            continue
        try:
            info = probe.stream_info(path, log)
            if name != "video_only.mp4":
                probe.probe_packets(path, log)
            if not info.has_audio and name != "video_only.mp4":
                log.add("missing_audio_track", f"{path} has no audio stream",
                        str(path), 0, -1)
        except MediaBatchError as exc:
            log.add(exc.error.code, exc.error.message, exc.error.source,
                    exc.error.source_offset, -1)

    boundaries: list[tuple[int, int, str, int, int]] = [
        (c.start_ms, c.end_ms, c.text, c.index, c.source_offset) for c in cues]
    extra = _timecode_cut_ms(workdir, log)
    if extra:
        split = _split_at_cuts(cues, extra, log)
        if len(split) == len(cues):
            log.add("timecode_ignored",
                    f"{len(extra)} timecode cut(s) fell outside every cue window; "
                    f"no chunk was split",
                    str(inp / "timecodes_25.txt"), 0, -1)
        boundaries = split

    boundaries.sort(key=lambda b: (b[0], b[1]))
    plan: list[ChunkPlan] = []
    last_end = -1
    for start, end, text, group, source_offset in boundaries:
        if end <= start:
            log.add("empty_chunk", f"chunk at {start}ms..{end}ms has "
                    f"non-positive duration; skipped",
                    str(inp / "program.srt"), source_offset, len(plan))
            continue
        if start < last_end:
            # Never trim a cue into a partial tail: a surviving tail can be
            # pure audio from the following cue and would be deduplicated
            # against it. Overlapping cues are kept whole, in source order.
            log.add("overlap", f"chunk {len(plan)} starts at {start}ms before "
                    f"the previous chunk ends at {last_end}ms; kept whole",
                    str(inp / "program.srt"), source_offset, len(plan))
        snap = _nearest(video_times, end)
        plan.append(ChunkPlan(len(plan), start, end, int(snap),
                              int(snap) - end, text, group))
        last_end = max(last_end, end)
    return plan


def measure_drift(media: str | Path, log: ErrorLog) -> timestamps.DriftReport:
    """Estimate container-vs-audio clock drift and log untrusted intervals."""
    media = Path(media)
    if not media.exists():
        return timestamps.DriftReport(Fraction(0), Fraction(0), [], True)
    anchors, times, rate = probe.audio_clock_anchors(media, log)
    report = timestamps.estimate_drift(anchors, times, rate or SAMPLE_RATE)
    if not report.trusted:
        for start, end in report.untrusted:
            log.add("untrusted_drift",
                    f"clock drift {float(report.ms_per_minute):.1f} ms/min between "
                    f"samples {start}..{end} exceeds 100 ms/min; interval untrusted",
                    str(media), 0, start)
    return report


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
                fh.truncate()
            fh.write(data)
            fh.flush()
            os.fsync(fh.fileno())
    except OSError as exc:
        raise MediaBatchError("io_error", f"write failed on {path}: {exc}",
                              str(path), offset if offset is not None else -1)


def _truncate_pcm(path: Path, size: int) -> None:
    try:
        with open(path, "r+b") as fh:
            fh.truncate(size)
            fh.flush()
            os.fsync(fh.fileno())
    except OSError as exc:
        raise MediaBatchError("io_error", f"truncate failed on {path}: {exc}",
                              str(path), size)


def _fingerprint_chunk(samples, sample_rate: int) -> str:
    return analyze.audio_fingerprint(samples, sample_rate)


def _recover(work: Path, pcm_path: Path, plan: list[ChunkPlan],
             index: CheckpointIndex, report: dict) -> int:
    t0 = time.monotonic()
    records = index.committed_chunks()
    valid, corrupt = verify_chunks(records, pcm_path)
    if corrupt is not None:
        index.rollback_from(corrupt.seq)
        _truncate_pcm(pcm_path, corrupt.output_offset)
        report["rolled_back_chunks"] = len(records) - len(valid)
    report["recovery_seconds"] = round(time.monotonic() - t0, 6)
    start = len(valid)
    report["reprocessed_chunks"] = [
        p.seq for p in plan if p.seq >= start]
    return start


def _prune_fingerprints(fp_path: Path, start_seq: int) -> None:
    entries = _load_json(fp_path, [])
    kept = [e for e in entries if e["seq"] < start_seq]
    _save_json(fp_path, kept)


def run(workdir: str | Path, interrupt_after: int | None = None,
        resume: bool = False) -> dict:
    workdir = Path(workdir)
    work = workdir / "work"
    work.mkdir(parents=True, exist_ok=True)
    pcm_path = work / "output.pcm"
    fp_path = work / "fingerprints.json"
    plan_path = work / "plan.json"
    err_path = work / "errors.json"
    index_path = work / "index.db"
    report: dict = {"recovery_seconds": 0.0, "reprocessed_chunks": []}

    if resume:
        if not plan_path.exists():
            raise MediaBatchError("no_checkpoint",
                                  "nothing to resume: no plan found", str(workdir), 0)
        plan = [ChunkPlan(**c) for c in _load_json(plan_path, [])]
        log = ErrorLog.load(err_path)
        index = CheckpointIndex(index_path)
        start_seq = _recover(work, pcm_path, plan, index, report)
        _prune_fingerprints(fp_path, start_seq)
    else:
        for p in (index_path, pcm_path, fp_path, plan_path, err_path):
            p.unlink(missing_ok=True)
        log = ErrorLog()
        drift_reports: list = []
        plan = build_plan(workdir, log, drift_reports)
        _save_json(plan_path, [asdict(c) for c in plan])
        index = CheckpointIndex(index_path)
        index.set_meta("plan_chunks", str(len(plan)))
        index.set_meta("drift", json.dumps([
            {"source": src,
             "ms_per_minute": float(rep.ms_per_minute),
             "drift_ppm": float(rep.drift_ppm * 1_000_000),
             "trusted": rep.trusted}
            for src, rep in drift_reports]))
        start_seq = 0
        report["reprocessed_chunks"] = [p.seq for p in plan]

    fingerprints: list[dict] = _load_json(fp_path, [])
    by_seq = {f["seq"]: f for f in fingerprints}
    groups = {c.seq: c.group for c in plan}
    reps: list[tuple[int, str]] = [(f["seq"], f["fingerprint"])
                                   for f in fingerprints if f["duplicate_of"] is None]
    committed = start_seq
    source = workdir / "input" / "program.wav"
    try:
        with sf.SoundFile(str(source)) as snd:
            for chunk in plan[start_seq:]:
                s0 = timebase.ms_to_samples(chunk.start_ms, SAMPLE_RATE)
                s1 = timebase.ms_to_samples(chunk.end_ms, SAMPLE_RATE)
                snd.seek(min(s0, len(snd)))
                samples = snd.read(max(0, min(s1, len(snd)) - s0),
                                   dtype="int16", always_2d=False)
                if samples.ndim > 1:
                    samples = samples.mean(axis=1).astype("int16")
                raw = samples.tobytes()
                in_sha = hashlib.sha256(raw).hexdigest()
                fp = _fingerprint_chunk(samples, SAMPLE_RATE)
                duplicate_of = None
                for rseq, rfp in reps:
                    if groups.get(rseq) == chunk.group:
                        continue  # pieces of the same cue carry distinct audio
                    if (rfp == fp
                            or analyze.fingerprint_similarity(rfp, fp) >= DUP_THRESHOLD):
                        duplicate_of = rseq
                        break
                # A duplicate contributes no output bytes: its offset is the
                # current end of the PCM file and its length is zero, which
                # keeps the checkpoint table a complete per-chunk record
                # without duplicating audio in the output.
                offset = pcm_path.stat().st_size if pcm_path.exists() else 0
                payload = b"" if duplicate_of is not None else raw
                _write_pcm(pcm_path, payload)
                if duplicate_of is None:
                    reps.append((chunk.seq, fp))
                fingerprints.append({"seq": chunk.seq, "fingerprint": fp,
                                     "duplicate_of": duplicate_of,
                                     "start_ms": chunk.start_ms,
                                     "end_ms": chunk.end_ms,
                                     "group": chunk.group})
                by_seq[chunk.seq] = fingerprints[-1]
                _save_json(fp_path, fingerprints)
                index.commit_chunk(ChunkRecord(
                    chunk.seq, in_sha, offset, len(payload),
                    hashlib.sha256(payload).hexdigest(), index.next_txn_id()))
                committed = chunk.seq + 1
                if interrupt_after is not None and committed >= interrupt_after:
                    raise PipelineInterrupted(committed)
    finally:
        log.save(err_path)
        index.close()

    source_log = ErrorLog.load(err_path)
    silence_ms = analyze.scan_silence_file(source, SAMPLE_RATE)
    final = workdir / "output" / "program.out.wav"
    out_sha = finalize_wav(pcm_path, final, SAMPLE_RATE)
    with CheckpointIndex(index_path) as final_index:
        final_index.set_meta("output_sha256", out_sha)
        drift_meta = final_index.get_meta("drift", "[]")
    drift = json.loads(drift_meta) if drift_meta else []

    by_seq = {f["seq"]: f for f in fingerprints}
    deviations = [
        {"seq": p.seq, "end_ms": p.end_ms, "snap_ms": p.snap_ms,
         "deviation_ms": p.deviation_ms}
        for p in plan if p.deviation_ms != 0]
    report.update({
        "chunks_committed": committed,
        "output_sha256": out_sha,
        "boundary_deviations": deviations,
        "duplicates": {seq: f["duplicate_of"] for seq, f in by_seq.items()
                       if f["duplicate_of"] is not None},
        "representatives": {seq: {"start_ms": f["start_ms"], "end_ms": f["end_ms"]}
                            for seq, f in by_seq.items() if f["duplicate_of"] is None},
        "silence_ms": [list(iv) for iv in silence_ms],
        "drift": drift,
        "subtitle_errors": [e.to_dict() for e in source_log
                            if e.source.endswith((".srt", ".vtt"))],
        "errors": [e.to_dict() for e in source_log],
    })
    _save_json(workdir / "output" / "manifest.json", report)
    return report
