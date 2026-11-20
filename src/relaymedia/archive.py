"""Streaming sink and final archival stage.

:class:`StreamSink` consumes bounded audio blocks as they arrive, never holding
a whole source in memory: PCM is appended straight into a soundfile WAV,
silence windows and fingerprint hashes are updated incrementally per fixed
time window, and a small per-node tail is carried only to finish the current
window. After every source is drained :func:`finalize` binds subtitles and the
timeline, performs sample-exact segment cuts, audits colour evidence and marks
the remaining checkpoints.
"""

from __future__ import annotations

import datetime
from dataclasses import asdict, dataclass
from pathlib import Path

import numpy as np
import soundfile as sf

from .audio import SilenceInterval
from .checkpoint import Checkpoint
from .clock import load_timecode_manifest
from .errors import ErrorRecord, Stage
from .fingerprint import Fingerprint, FingerprintIndex, register_and_match
from .segments import build_segments, cut_pcm
from .subtitles import parse_subtitles
from .fingerprint import _window_hashes  # reused so streaming matches batch hash


def _now() -> str:
    return datetime.datetime.now(datetime.UTC).isoformat()


class _NodeWriter:
    """Incremental WAV writer plus streaming silence/fingerprint state."""

    def __init__(self, path: Path, rate: int, channels: int,
                 silence_win: int, fp_win: int, threshold: float) -> None:
        self.file = sf.SoundFile(str(path), mode="w", samplerate=rate,
                                 channels=channels, subtype="PCM_16")
        self.path = path
        self.rate = rate
        self.channels = channels
        self.silence_win = silence_win
        self.fp_win = fp_win
        self.threshold = threshold
        self.silence_tail = np.empty((0, channels), dtype=np.float32)
        self.fp_tail = np.empty((0, channels), dtype=np.float32)
        self.hashes: list[int] = []
        self.silence: list[SilenceInterval] = []
        self._silent_run_start = -1
        self._silent_run_end = -1  # absolute end sample of the open run
        self._silent_run_rms = 0.0
        self._window_index = 0  # count of completed fixed-size windows
        self.samples = 0

    def push(self, block: np.ndarray) -> None:
        if block.ndim == 1:
            block = block[:, None]
        self.file.write(block)
        self.samples += block.shape[0]
        self._update_silence(block)
        self._update_fingerprint(block)

    def _update_silence(self, block: np.ndarray) -> None:
        data = np.concatenate([self.silence_tail, block], axis=0)
        usable = (len(data) // self.silence_win) * self.silence_win
        base = self.samples - len(data)
        for start in range(0, usable, self.silence_win):
            window = data[start:start + self.silence_win]
            mono = window.mean(axis=1)
            rms = float(np.sqrt(np.mean(np.square(mono, dtype=np.float64))))
            abs_start = base + start
            abs_end = abs_start + self.silence_win
            if rms < self.threshold:
                if self._silent_run_start >= 0 and abs_start == self._silent_run_end:
                    self._silent_run_end = abs_end  # extend abutting run
                    self._silent_run_rms = max(self._silent_run_rms, rms)
                else:
                    if self._silent_run_start >= 0:
                        self._flush_silent_run(self._silent_run_end)
                    self._silent_run_start = abs_start
                    self._silent_run_end = abs_end
                    self._silent_run_rms = rms
            elif self._silent_run_start >= 0:
                self._flush_silent_run(self._silent_run_end)
            self._window_index += 1
        self.silence_tail = data[usable:].copy()

    def _flush_silent_run(self, end_sample: int) -> None:
        self.silence.append(SilenceInterval(
            self._silent_run_start, end_sample,
            self._silent_run_start / self.rate, end_sample / self.rate,
            self._silent_run_rms))
        self._silent_run_start = -1
        self._silent_run_end = -1
        self._silent_run_rms = 0.0

    def _update_fingerprint(self, block: np.ndarray) -> None:
        data = np.concatenate([self.fp_tail, block], axis=0)
        usable = (len(data) // self.fp_win) * self.fp_win
        if usable:
            self.hashes.extend(_window_hashes(
                data[:usable], self.rate, 1000.0 * self.fp_win / self.rate))
        self.fp_tail = data[usable:].copy()

    def close(self) -> None:
        # A trailing partial window still counts as content; finish any open
        # silent run against the total sample count so nothing is dropped.
        if len(self.silence_tail) and self._silent_run_start >= 0:
            mono = self.silence_tail.mean(axis=1)
            rms = float(np.sqrt(np.mean(np.square(mono, dtype=np.float64))))
            if rms < self.threshold:
                self._silent_run_end = self.samples
            self._flush_silent_run(self._silent_run_end)
        elif self._silent_run_start >= 0:
            self._flush_silent_run(self._silent_run_end)
        if len(self.fp_tail):
            self.hashes.extend(_window_hashes(self.fp_tail, self.rate,
                                               1000.0 * self.fp_win / self.rate))
        self.file.close()


class StreamSink:
    """Receives blocks node-by-node and owns the incremental output state."""

    def __init__(self, work: Path, config) -> None:
        self.work = work
        self.config = config
        self.writers: dict[str, _NodeWriter] = {}
        self.order: list[str] = []
        self.errors: list[ErrorRecord] = []
        rate = config.out_rate
        self.silence_win = max(1, int(round(rate * config.window_ms / 1000.0)))
        self.fp_win = max(1, int(round(rate * config.fp_window_ms / 1000.0)))

    def push(self, node_id: str, block) -> None:
        writer = self.writers.get(node_id)
        if writer is None:
            path = self.work / f"{node_id}.wav"
            writer = _NodeWriter(
                path, self.config.out_rate, self.config.out_channels,
                self.silence_win, self.fp_win, self.config.silence_threshold,
            )
            self.writers[node_id] = writer
            self.order.append(node_id)
        writer.push(block.data)

    def finish_node(self) -> None:
        for writer in self.writers.values():
            if not writer.file.closed:
                writer.close()
    def fingerprints(self) -> list[Fingerprint]:
        cfg = self.config
        return [
            Fingerprint(
                source=node, sample_rate=cfg.out_rate, window_ms=cfg.fp_window_ms,
                hashes=tuple(self.writers[node].hashes),
                window_seconds=cfg.fp_window_ms / 1000.0,
            )
            for node in self.order
        ]


def _collect_errors(result, *lists: list[ErrorRecord]) -> None:
    for items in lists:
        result.errors.extend(items)
    known = [e.offset_bytes for e in result.errors if e.offset_bytes >= 0]
    result.first_error_offset_bytes = min(known) if known else -1
    result.bad_record_count = len(result.errors)


def finalize(result, sink: StreamSink, checkpoint: Checkpoint, config,
             sources: list[tuple[str, str]], subtitle_path: str,
             video_paths: list[str], probe_color_fn, open_video_fn,
             timecode_path: str | None = None) -> None:
    sink.finish_node()
    work = sink.work

    # Optional editorial timecode manifest: fractional rates, negative offset,
    # day wrap, repeated and unconvertible stamps. Errors are located.
    timecode_errors: list[ErrorRecord] = []
    timecode_records = []
    if timecode_path:
        timecode_records, timecode_errors, clock = load_timecode_manifest(timecode_path)
        checkpoint.mark_stage(Stage.TIMELINE, {
            "timecode_wraps": clock.wraps,
            "converted": [r.line for r in timecode_records if r.unified_seconds is not None],
        }, completed_at=_now())

    # Subtitle + timeline stage.
    track = parse_subtitles(subtitle_path)
    checkpoint.mark_stage(Stage.SUBTITLE, {
        "cues": [(c.raw_start, c.raw_end) for c in track.cues],
        "errors": [e.to_dict() for e in track.errors],
    }, completed_at=_now())
    plan = build_segments(track, config.out_rate, max_boundary_ms=20.0,
                          timeline_offset=config.timeline_offset)
    checkpoint.mark_stage(Stage.TIMELINE, [s.to_dict() for s in plan.segments],
                          completed_at=_now())

    # Register streaming fingerprints (earliest node is the dedup authority).
    index = FingerprintIndex(window_ms=config.fp_window_ms)
    for fp in sink.fingerprints():
        register_and_match(index, fp)

    output_files = [str(w.path) for w in sink.writers.values()]
    all_silence: list[SilenceInterval] = []
    for node in sink.order:
        all_silence.extend(sink.writers[node].silence)

    # Sample-exact cuts from the primary node's finished WAV (re-read, bounded).
    primary_node = sources[0][0] if sources else None
    if primary_node and primary_node in sink.writers:
        pcm, _ = sf.read(str(sink.writers[primary_node].path), dtype="float32",
                         always_2d=True)
        for segment in plan.segments:
            if segment.end_sample <= pcm.shape[0]:
                seg_path = work / "segments" / f"{segment.id}.wav"
                sf.write(str(seg_path), cut_pcm(pcm, segment), config.out_rate,
                         subtype="PCM_16")
                output_files.append(str(seg_path))
        result.segments = plan.segments
        result.segment_count = len(plan.segments)

    for video in video_paths:
        try:
            container = open_video_fn(video)
            report = probe_color_fn(video, container.streams.video[0])
            container.close()
            result.color_reports.append(report)
        except Exception as exc:
            result.errors.append(ErrorRecord(
                stage=Stage.COLOR.value, kind="MediaError",
                message=f"cannot probe video {video}: {exc}", source=video))

    result.silence_intervals = all_silence
    result.duplicates = index.matches
    result.duplicate_count = index.duplicate_count
    result.output_files = sorted(output_files)
    _collect_errors(result, track.errors, plan.errors, sink.errors,
                    timecode_errors,
                    *(r.conflicts for r in result.color_reports))
    checkpoint.mark_stage(Stage.FINGERPRINT,
                          [asdict(m) for m in index.matches],
                          completed_at=_now())
    checkpoint.mark_stage(Stage.WRITE, {"files": result.output_files},
                          completed_at=_now())
