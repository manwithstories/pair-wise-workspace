"""Shared analyze pipeline used by the analyze / batch / selftest commands."""
from __future__ import annotations

from . import detector, framing, refine, store, wavio


class InvariantError(Exception):
    """Segment/silence tiling invariant violated; maps to exit code 3."""


def _round6(value):
    return round(float(value), 6)


def run_pipeline(path, params):
    """decode -> frame -> detect -> refine -> silences -> invariants."""
    samples, rate = wavio.read_wav(path)
    total = int(samples.shape[0])
    frame_db, frame_len = framing.frame_dbfs(samples, rate, params["frame_ms"])
    noise_floor = detector.estimate_noise_floor(frame_db)
    candidates, thresholds = detector.detect_events(
        frame_db, frame_len, total, rate,
        min_event_ms=params["min_event_ms"],
        min_silence_ms=params["min_silence_ms"],
        noise_floor_db=noise_floor)
    segments = refine.refine_segments(
        samples, rate, candidates, noise_floor,
        thresholds["enter_threshold_dbfs"],
        fade_guard_ms=params["fade_guard_ms"])
    silences = detector.compute_silences(segments, total)
    problems = detector.check_invariants(segments, silences, total)
    if problems:
        raise InvariantError("; ".join(problems))
    silence_total = sum(s.duration_samples for s in silences)
    result = {
        "sample_rate": rate,
        "total_samples": total,
        "segment_count": len(segments),
        "silence_total_samples": silence_total,
        "silence_total_ms": _round6(silence_total / rate * 1000.0),
        "noise_floor_dbfs": _round6(noise_floor),
        "threshold_dbfs": _round6(thresholds["enter_threshold_dbfs"]),
        "segments": [{
            "ordinal": s.ordinal,
            "start_sample": s.start_sample,
            "end_sample": s.end_sample,
            "fade_in_samples": s.fade_in_samples,
            "fade_out_samples": s.fade_out_samples,
        } for s in segments],
        "silences": [{
            "start_sample": s.start_sample,
            "end_sample": s.end_sample,
            "duration_samples": s.duration_samples,
        } for s in silences],
    }
    return result, segments, silences


def persist_run(db_path, path, params, result, segments, silences):
    conn = store.open_db(db_path)
    try:
        return store.save_run(
            conn, sha256=wavio.file_sha256(path), source_path=str(path),
            sample_rate=result["sample_rate"],
            total_samples=result["total_samples"], params=params,
            noise_floor_dbfs=result["noise_floor_dbfs"],
            threshold_dbfs=result["threshold_dbfs"],
            segments=segments, silences=silences)
    finally:
        store.close_db(conn)
