"""Compliance checks and run reporting (text / JSON)."""
from __future__ import annotations

DEFAULT_MIN_GAP_MS = 150.0
DEFAULT_MAX_GAP_MS = 5000.0
DEFAULT_MIN_EVENT_MS = 150.0
DEFAULT_MARGIN_MS = 20.0


def check_compliance(run, *, min_gap_ms=DEFAULT_MIN_GAP_MS,
                     max_gap_ms=DEFAULT_MAX_GAP_MS,
                     min_event_ms=DEFAULT_MIN_EVENT_MS,
                     margin_ms=DEFAULT_MARGIN_MS):
    """Validate gap window, event length and head/tail margins."""
    rate = run["sample_rate"]
    total = run["total_samples"]
    violations = []

    def ms(samples):
        return round(samples / rate * 1000.0, 6)

    min_gap = min_gap_ms * rate / 1000.0
    max_gap = max_gap_ms * rate / 1000.0
    min_event = min_event_ms * rate / 1000.0
    margin = margin_ms * rate / 1000.0
    for sil in run["silences"]:
        interior = sil["start_sample"] > 0 and sil["end_sample"] < total
        dur = sil["duration_samples"]
        if interior and dur < min_gap:
            violations.append({
                "type": "gap_too_short", "silence_ordinal": sil["ordinal"],
                "start_sample": sil["start_sample"],
                "duration_ms": ms(dur), "limit_ms": min_gap_ms})
        if interior and dur > max_gap:
            violations.append({
                "type": "gap_too_long", "silence_ordinal": sil["ordinal"],
                "start_sample": sil["start_sample"],
                "duration_ms": ms(dur), "limit_ms": max_gap_ms})
    for seg in run["segments"]:
        dur = seg["end_sample"] - seg["start_sample"]
        if dur < min_event:
            violations.append({
                "type": "event_too_short", "segment_ordinal": seg["ordinal"],
                "start_sample": seg["start_sample"],
                "duration_ms": ms(dur), "limit_ms": min_event_ms})
    segs = run["segments"]
    head = segs[0]["start_sample"] if segs else total
    tail = total - segs[-1]["end_sample"] if segs else total
    if head < margin:
        violations.append({"type": "head_margin_short",
                           "duration_ms": ms(head), "limit_ms": margin_ms})
    if tail < margin:
        violations.append({"type": "tail_margin_short",
                           "duration_ms": ms(tail), "limit_ms": margin_ms})
    return violations


def silence_total_samples(run):
    return sum(s["duration_samples"] for s in run["silences"])


def format_text(run, violations):
    rate = run["sample_rate"]
    lines = [
        f"run {run['run_id']} | source={run['source_path']} | "
        f"rate={rate} | total_samples={run['total_samples']}",
        f"noise_floor_dbfs={run['noise_floor_dbfs']} "
        f"threshold_dbfs={run['threshold_dbfs']}",
        f"segments: {len(run['segments'])}",
    ]
    for seg in run["segments"]:
        dur = seg["end_sample"] - seg["start_sample"]
        lines.append(
            f"  seg{seg['ordinal']:03d} start={seg['start_sample']} "
            f"end={seg['end_sample']} dur={dur} "
            f"({round(dur / rate * 1000.0, 6)} ms) "
            f"fade_in={seg['fade_in_samples']} "
            f"fade_out={seg['fade_out_samples']}")
    total_sil = silence_total_samples(run)
    lines.append(
        f"silences: {len(run['silences'])} | "
        f"silence_total_samples={total_sil} | "
        f"silence_total_ms={round(total_sil / rate * 1000.0, 6)}")
    lines.append(f"violations: {len(violations)}")
    for item in violations:
        lines.append(f"  {item['type']} {item}")
    return "\n".join(lines)


def format_json(run, violations):
    rate = run["sample_rate"]
    total_sil = silence_total_samples(run)
    return {
        "run_id": run["run_id"],
        "source_path": run["source_path"],
        "sha256": run["sha256"],
        "sample_rate": rate,
        "total_samples": run["total_samples"],
        "noise_floor_dbfs": run["noise_floor_dbfs"],
        "threshold_dbfs": run["threshold_dbfs"],
        "segment_count": len(run["segments"]),
        "silence_total_samples": total_sil,
        "silence_total_ms": round(total_sil / rate * 1000.0, 6),
        "segments": run["segments"],
        "silences": run["silences"],
        "violations": violations,
    }
