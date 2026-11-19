"""Pipeline orchestration: parse -> transform -> index -> validate.

Each track is processed independently: a failure on one track is recorded and
the other tracks still complete. All outputs land under the user-specified
output directory; the input directory is never modified or overwritten.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass, field

import numpy as np

from . import annexb, index as index_mod, metrics, probe
from .clock import TICKS_PER_MS, ms_to_ticks
from .errors import ErrorLog, InputError, Recovery, Stage
from .packets import PacketStream, parse_stream
from .timeline import (
    AudioTimeline,
    VideoTimeline,
    build_audio_timeline,
    build_video_timeline,
    read_audio,
    reassemble,
)


@dataclass
class PipelineResult:
    summary: dict
    errors: ErrorLog
    videos: list[VideoTimeline] = field(default_factory=list)
    audios: list[AudioTimeline] = field(default_factory=list)
    seek_failures: int = 0


def _validate_dirs(input_dir: str, output_dir: str) -> tuple[str, str]:
    if not os.path.isdir(input_dir):
        raise InputError(f"input directory not readable: {input_dir}")
    in_abs = os.path.realpath(input_dir)
    out_abs = os.path.realpath(output_dir)
    if in_abs == out_abs or out_abs.startswith(in_abs + os.sep):
        raise InputError("output directory must not be the input directory "
                         "or nested inside it (refusing to overwrite sources)")
    os.makedirs(out_abs, exist_ok=True)
    return in_abs, out_abs


def _load_manifest(input_dir: str) -> dict:
    path = os.path.join(input_dir, "manifest.json")
    if not os.path.isfile(path):
        raise InputError(f"manifest missing: {path}")
    try:
        with open(path, "r", encoding="utf-8") as f:
            manifest = json.load(f)
    except (json.JSONDecodeError, OSError) as exc:
        raise InputError(f"manifest unreadable: {exc}") from exc
    if not isinstance(manifest, dict) or "video" not in manifest:
        raise InputError("manifest missing required 'video' section")
    return manifest


def _process_video(
    spec: dict,
    input_dir: str,
    work_dir: str,
    out_dir: str,
    reorder_window_ms: int,
    log: ErrorLog,
) -> VideoTimeline | None:
    track = spec["track"]
    codec = spec["codec"]
    if codec not in annexb.CODECS:
        raise InputError(f"unsupported codec {codec!r} for track {track}")
    pkt_path = os.path.join(input_dir, spec["packet_file"])
    if not os.path.isfile(pkt_path):
        raise InputError(f"packet file missing: {pkt_path}")

    stream = parse_stream(pkt_path, track, log, reorder_window_ms)
    es_work = os.path.join(work_dir, f"{track}.{codec}.recovered.es")
    spans = reassemble(stream, es_work)
    stream_size = os.path.getsize(es_work)

    scan_res = annexb.scan(es_work, codec, tail_complete=True)
    for err in scan_res.errors:
        err.track = track
        log.records.append(err)

    tl = build_video_timeline(track, codec, stream, scan_res.units, spans,
                              es_work, stream_size, log)

    # --- index: validate supplied one, then rebuild from real keyframes ----
    index_path = os.path.join(input_dir, spec["index_file"])
    supplied: list[index_mod.IndexEntry] = []
    if os.path.isfile(index_path):
        try:
            supplied = index_mod.load_index(index_path)
        except (json.JSONDecodeError, KeyError, ValueError, OSError) as exc:
            log.add(Stage.INDEX, track, f"supplied index unreadable: {exc}",
                    recovery=Recovery.UNRECOVERABLE)
    key_offsets = {u.offset for u in scan_res.units
                   if u.is_keyframe and u.complete}
    valid = index_mod.validate_index(supplied, stream_size, key_offsets,
                                     log, track) if supplied else []
    frame_of = {fr.stream_offset: fr.index for fr in tl.frames}
    ts_of = {fr.stream_offset: fr.ref_ts for fr in tl.frames}
    key_units = [u for u in scan_res.units if u.is_keyframe and u.complete]
    rebuilt = index_mod.rebuild_index(key_units, frame_of, ts_of)
    failures = index_mod.verify_seeks(rebuilt, key_offsets, log, track)

    out_index = os.path.join(out_dir, f"{track}.{codec}.index.repaired.json")
    with open(out_index, "w", encoding="utf-8") as f:
        f.write(rebuilt.to_json(track))
    if failures:  # keep a copy of the original alongside the failure record
        with open(os.path.join(out_dir, f"{track}.index.original.json"),
                  "w", encoding="utf-8") as f:
            json.dump([e.to_dict() for e in supplied], f, indent=2)
    # also publish the recovered elementary stream
    final_es = os.path.join(out_dir, f"{track}.{codec}.recovered.es")
    with open(es_work, "rb") as src, open(final_es, "wb") as dst:
        dst.write(src.read())
    tl.stream_path = final_es
    tl.probe = probe.probe_stream(final_es, codec)
    return tl, failures, stream


def _process_audio(
    spec: dict,
    input_dir: str,
    work_dir: str,
    out_dir: str,
    reorder_window_ms: int,
    ref_audio: np.ndarray | None,
    video_duration_ticks: int,
    log: ErrorLog,
) -> AudioTimeline | None:
    track = spec["track"]
    pkt_path = os.path.join(input_dir, spec["packet_file"])
    if not os.path.isfile(pkt_path):
        raise InputError(f"packet file missing: {pkt_path}")
    stream = parse_stream(pkt_path, track, log, reorder_window_ms)
    wav_path = os.path.join(work_dir, f"{track}.recovered.wav")
    expected = ms_to_ticks(spec.get("expected_offset_ms", 0))
    tl = build_audio_timeline(track, stream, wav_path, ref_audio, expected,
                              video_duration_ticks)
    if tl.overrun_ticks:
        log.add(Stage.VALIDATE, track,
                f"audio extends {tl.overrun_ticks // TICKS_PER_MS} ms past "
                "video timeline; retained, not clipped",
                recovery=Recovery.NONE)
    final_wav = os.path.join(out_dir, f"{track}.recovered.wav")
    with open(wav_path, "rb") as src, open(final_wav, "wb") as dst:
        dst.write(src.read())
    tl.path = final_wav
    return tl, stream


def run(input_dir: str, output_dir: str,
        reorder_window_ms: int = 120) -> PipelineResult:
    """Execute the full repair pipeline and return metrics + structured errors."""
    if not 0 < reorder_window_ms <= 10_000:
        raise InputError("reorder_window_ms must be within (0, 10000]")
    in_abs, out_abs = _validate_dirs(input_dir, output_dir)
    manifest = _load_manifest(in_abs)
    log = ErrorLog()
    work_dir = os.path.join(out_abs, ".work")
    os.makedirs(work_dir, exist_ok=True)

    videos: list[VideoTimeline] = []
    streams: list[PacketStream] = []
    seek_failures = 0
    for spec in manifest.get("video", []):
        try:
            tl, failures, stream = _process_video(
                spec, in_abs, work_dir, out_abs, reorder_window_ms, log)
            videos.append(tl)
            streams.append(stream)
            seek_failures += failures
        except InputError:
            raise
        except Exception as exc:  # one track's failure never hides the others
            log.add(Stage.PARSE, spec.get("track", "?"),
                    f"track processing failed: {type(exc).__name__}: {exc}",
                    recovery=Recovery.UNRECOVERABLE)

    # reference audio for offset estimation
    ref_audio = None
    for spec in manifest.get("audio", []):
        if spec.get("reference"):
            ref_path = os.path.join(in_abs, spec["track"] + ".wav")
            if os.path.isfile(ref_path):
                ref_audio, _, _ = read_audio(ref_path)
            break
    duration = max((v.duration_ticks for v in videos), default=0)
    audios: list[AudioTimeline] = []
    for spec in manifest.get("audio", []):
        try:
            tl, stream = _process_audio(
                spec, in_abs, work_dir, out_abs, reorder_window_ms,
                ref_audio, duration, log)
            audios.append(tl)
            streams.append(stream)
        except InputError:
            raise
        except Exception as exc:
            log.add(Stage.PARSE, spec.get("track", "?"),
                    f"track processing failed: {type(exc).__name__}: {exc}",
                    recovery=Recovery.UNRECOVERABLE)

    summary = metrics.summarize(streams, videos, audios, seek_failures)
    return PipelineResult(summary, log, videos, audios, seek_failures)
