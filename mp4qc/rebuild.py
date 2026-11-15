"""Rebuild sidx from real moof/mdat boundaries, then self-check."""
from __future__ import annotations

import os

from .boxio import BoxReader
from .frag import parse_moof
from .report import EXIT_OK, EXIT_RECHECK_FAILED, Report
from .sidx import Sidx, SidxReference, build_sidx, parse_sidx, reference_spans
from .verify import verify_file

__all__ = ["rebuild_file"]


def _subsegments(boxes):
    """Group each moof with its trailing mdat boxes -> [start, size]."""
    segments = []
    current = None
    for box in boxes:
        if box.type == "moof":
            current = [box.start, box.size]
            segments.append(current)
        elif box.type == "mdat" and current is not None:
            current[1] += box.size
        else:
            current = None
    return segments


def _segment_durations(reader, segments):
    durations = []
    for start, _size in segments:
        moof = parse_moof(reader, reader.box_at(start))
        durations.append(max((t.total_duration for t in moof.trafs), default=0))
    return durations


def rebuild_file(src, dst):
    """Returns (Report, exit_code); exit 4 when the recheck fails."""
    report = Report("rebuild", src)
    with BoxReader(src) as reader:
        boxes = list(reader.boxes())
        sidx_box = next((b for b in boxes if b.type == "sidx"), None)
        if sidx_box is None:
            report.add("error", "no sidx box found; nothing to rebuild")
            return report, 2
        old = parse_sidx(reader, sidx_box)
        old_spans = reference_spans(old, sidx_box.end)
        segments = _subsegments(boxes)
        durations = _segment_durations(reader, segments)
        refs = []
        for i, ((_start, size), dur) in enumerate(zip(segments, durations)):
            if i < len(old.references):
                prev = old.references[i]
                sap = prev.starts_with_sap
                sap_type = prev.sap_type
                sap_delta = prev.sap_delta_time
            else:
                sap, sap_type, sap_delta = True, 1, 0
            refs.append(SidxReference(0, size, dur, sap, sap_type, sap_delta))
        new_sidx = Sidx(old.version, old.reference_id, old.timescale,
                        old.earliest_presentation_time, 0, refs)
        sidx_bytes = build_sidx(new_sidx)  # may bump to version 1
        items = []
        new_moof_pos = []
        pos = 0
        sidx_new_end = None
        for box in boxes:
            if box.type == "free":
                continue
            if box is sidx_box:
                items.append(("sidx", None, len(sidx_bytes)))
                pos += len(sidx_bytes)
                sidx_new_end = pos
            else:
                if box.type == "moof":
                    new_moof_pos.append(pos)
                items.append(("copy", box.start, box.size))
                pos += box.size
        first_offset = (new_moof_pos[0] - sidx_new_end) if new_moof_pos else 0
        new_sidx.first_offset = first_offset
        sidx_bytes = build_sidx(new_sidx)  # same length, final first_offset
        length_after = pos
        os.makedirs(os.path.dirname(os.path.abspath(dst)), exist_ok=True)
        with open(dst, "wb") as out:
            for kind, start, size in items:
                if kind == "sidx":
                    out.write(sidx_bytes)
                else:
                    reader.copy_range(out, start, size)
    boundaries = []
    for i, (start, size) in enumerate(segments):
        if i < len(old_spans):
            old_offset, old_size = old_spans[i][0], old_spans[i][1]
        else:
            old_offset, old_size = None, None
        boundaries.append({
            "index": i,
            "old_offset": old_offset,
            "new_offset": new_moof_pos[i] if i < len(new_moof_pos) else None,
            "old_size": old_size,
            "new_size": size,
        })
    length_before = os.path.getsize(src)
    report.add("info", "rewrote sidx: %d references from real boundaries"
               % len(refs))
    report.add("info", "file length %d -> %d (delta %+d bytes)"
               % (length_before, length_after, length_after - length_before))
    recheck = verify_file(dst)
    recheck_ok = (recheck.data["offset_violations"] == 0
                  and recheck.data["sidx_references"] == len(segments))
    if recheck_ok:
        report.add("info", "recheck passed: 0 offset violations")
    else:
        report.add("error", "recheck failed: %d violations remain"
                   % recheck.data["offset_violations"])
    report.data.update({
        "references": len(refs),
        "length_before": length_before,
        "length_after": length_after,
        "length_delta": length_after - length_before,
        "boundaries": boundaries,
        "recheck": {
            "sidx_references": recheck.data["sidx_references"],
            "offset_violations": recheck.data["offset_violations"],
            "max_drift_bytes": recheck.data["max_drift_bytes"],
        },
    })
    return report, EXIT_OK if recheck_ok else EXIT_RECHECK_FAILED
