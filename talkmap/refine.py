"""Boundary refinement: short-run merging, fades, clamping and the pipeline."""

from __future__ import annotations

from dataclasses import dataclass
from pathlib import Path

import numpy as np

from .config import Params
from .detect import label_frames, runs
from .framing import Framer, rms_to_dbfs
from .noise import estimate_noise_db
from .wavio import WavReader


@dataclass(frozen=True)
class Segment:
    start_sample: int
    end_sample: int
    start_frame: int
    end_frame: int
    fade_in_ms: float | None
    fade_out_ms: float | None


def merge_speech_runs(raw_runs, hop_samples: int, sample_rate: int,
                      params: Params) -> list[tuple[int, int]]:
    """Apply the two spec rules in order:

    1. a silence run shorter than ``min_silence_ms`` joins the speech on both
       sides into one speech run;
    2. any remaining speech run shorter than ``min_speech_ms`` is absorbed into
       its nearest speech neighbour (dropped when it is the only run).
    """
    min_silence = params.min_silence_ms / 1000.0 * sample_rate
    min_speech = params.min_speech_ms / 1000.0 * sample_rate
    runs_list = [list(run) for run in raw_runs]
    joined: list[list[int]] = []
    for run in runs_list:
        start, end, label = run
        if (joined and label == 0 and joined[-1][2] == 1
                and (end - start) * hop_samples < min_silence):
            joined[-1][1] = end
            continue
        if joined and joined[-1][2] == label:
            joined[-1][1] = end
        else:
            joined.append(run)
    speech = [(s, e) for s, e, label in joined if label == 1]
    result: list[tuple[int, int]] = []
    for start, end in speech:
        if (end - start) * hop_samples >= min_speech:
            result.append((start, end))
            continue
        if not result:
            pending = (start, end)
            result.append(pending)
            continue
        prev_start, _ = result[-1]
        result[-1] = (prev_start, end)
    if result and (result[0][1] - result[0][0]) * hop_samples < min_speech \
            and len(result) > 1:
        result = result[1:]
    return result


def _fade_length(frame_db, noise_db, anchor, hop_ms, params, direction):
    """Measure how long the level takes to settle onto its stable plateau.

    Fade-in climbs forward from the entry anchor to the interior plateau;
    fade-out looks backward from the release anchor to the interior plateau.
    A median plateau window beyond the fade region keeps the estimate stable.
    Returns None when the span does not exceed ``fade_record_ms``.
    """
    cap = int(round(params.fade_record_ms * 2.0 / hop_ms)) + 2
    if direction == "in":
        plateau_lo = min(frame_db.size, anchor + cap)
        plateau_hi = min(frame_db.size, anchor + 2 * cap)
        if plateau_hi <= plateau_lo:
            plateau_lo, plateau_hi = anchor, min(frame_db.size, anchor + cap)
        plateau = float(np.median(frame_db[plateau_lo:plateau_hi]))
        stable = plateau - params.stable_drop_db
        last = anchor
        for step in range(cap):
            idx = anchor + step
            if idx >= frame_db.size:
                break
            last = idx
            if frame_db[idx] >= stable:
                break
        length_frames = last - anchor + 1
    else:
        plateau_hi = max(0, anchor - cap)
        plateau_lo = max(0, anchor - 2 * cap)
        if plateau_lo >= plateau_hi:
            plateau_lo, plateau_hi = max(0, anchor - cap), anchor + 1
        plateau = float(np.median(frame_db[plateau_lo:plateau_hi]))
        stable = plateau - params.stable_drop_db
        first = anchor
        for step in range(cap):
            idx = anchor - step
            if idx < 0:
                break
            first = idx
            if frame_db[idx] >= stable:
                break
        length_frames = anchor - first + 1
    length_ms = length_frames * hop_ms
    return length_ms if length_ms > params.fade_record_ms else None


def refine_segments(frame_db, noise_db, labels, framer: Framer,
                    total_samples: int, params: Params) -> list[Segment]:
    hop_ms = params.hop_ms
    hop_samples = framer.hop_samples
    frame_samples = framer.frame_samples
    tail_cap = max(1, round(params.fade_tail_max_ms / hop_ms))
    speech_runs = merge_speech_runs(runs(labels), hop_samples,
                                    framer.sample_rate, params)
    segments: list[Segment] = []
    for start, end in speech_runs:
        fade_in = _fade_length(frame_db, noise_db, start, hop_ms, params, "in")
        tail = 0
        while tail < tail_cap and end + tail < frame_db.size:
            if frame_db[end + tail] >= noise_db[end + tail] + params.fade_start_margin_db:
                tail += 1
            else:
                break
        extended_end = end + tail
        fade_out = _fade_length(frame_db, noise_db, extended_end - 1,
                                hop_ms, params, "out")
        start_sample = framer.hop_samples * start
        end_sample = min(total_samples,
                         framer.hop_samples * (extended_end - 1) + frame_samples)
        segments.append(Segment(
            start_sample=start_sample,
            end_sample=end_sample,
            start_frame=start,
            end_frame=extended_end,
            fade_in_ms=fade_in,
            fade_out_ms=fade_out,
        ))
    return segments


def run_pipeline(reader: WavReader, params: Params) -> tuple[list[Segment], int]:
    """Stream the WAV through framing, noise, detection and refinement."""
    params.validate()
    framer = Framer(reader.info.sample_rate, params.frame_ms, params.hop_ms)
    block_frames = max(1, reader.info.sample_rate // 4)
    levels: list[float] = []
    for block in reader.iter_mono_blocks(block_frames):
        for frame in framer.push(block):
            levels.append(frame.rms)
    framer.finish()
    rms = np.asarray(levels, dtype=np.float64)
    frame_db = rms_to_dbfs(rms)
    noise_db = estimate_noise_db(frame_db, params.hop_ms, params)
    labels = label_frames(frame_db, noise_db, params)
    segments = refine_segments(frame_db, noise_db, labels, framer,
                               reader.info.total_frames, params)
    return segments, reader.info.total_frames
