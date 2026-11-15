"""Cross-check sidx reference spans against real moof boundaries."""
from __future__ import annotations

from .boxio import BoxError, BoxReader
from .report import Report
from .sidx import parse_sidx, reference_spans
from .stbl import ENCRYPTED_SAMPLE_ENTRIES, iter_traks, read_codec, stbl_of

__all__ = ["verify_file"]


def _check_encryption(reader, moov, report):
    for trak, track_id in iter_traks(reader, moov):
        stbl = stbl_of(reader, trak)
        codec = read_codec(reader, stbl) if stbl is not None else None
        if codec in ENCRYPTED_SAMPLE_ENTRIES:
            report.add("warn",
                       "track %s uses encrypted sample entry %r; "
                       "metadata checked only, media not decoded"
                       % (track_id, codec))


def _expand(reader, sidx, sidx_end, depth):
    """Flatten reference spans; reference_type 1 recurses one level."""
    spans = []
    for offset, size, ref in reference_spans(sidx, sidx_end):
        if ref.reference_type == 1 and depth < 1:
            try:
                sub_box = reader.box_at(offset)
                if sub_box.type == "sidx":
                    sub = parse_sidx(reader, sub_box)
                    spans.extend(_expand(reader, sub, sub_box.end, depth + 1))
                    continue
            except BoxError:
                pass
        spans.append((offset, size))
    return spans


def verify_file(path, strict=False):
    report = Report("verify", path)
    with BoxReader(path) as reader:
        moofs = []
        sidx_boxes = []
        for box in reader.boxes():
            if box.type == "moof":
                moofs.append((box.start, box.size))
            elif box.type == "sidx":
                sidx_boxes.append(box)
            elif box.type == "moov":
                _check_encryption(reader, box, report)
        expected = []
        for sidx_box in sidx_boxes:
            sidx = parse_sidx(reader, sidx_box)
            expected.extend(_expand(reader, sidx, sidx_box.end, 0))
    if not sidx_boxes:
        report.add("info", "no sidx box found; nothing to verify")
    entries = []
    violations = 0
    max_drift = 0
    first_violation = None
    for i, (offset, size) in enumerate(expected):
        actual = moofs[i][0] if i < len(moofs) else None
        drift = None if actual is None else offset - actual
        ok = drift == 0
        if not ok:
            violations += 1
            if first_violation is None:
                first_violation = i
            if drift is not None:
                max_drift = max(max_drift, abs(drift))
            report.add("error", "reference %d offset drift %s bytes "
                       "(expected %d, actual moof at %s)"
                       % (i, drift, offset, actual))
        entries.append({"index": i, "expected_offset": offset,
                        "actual_offset": actual, "referenced_size": size,
                        "drift_bytes": drift, "ok": ok})
    if violations == 0 and expected:
        report.add("info", "all %d sidx references match moof boundaries"
                   % len(expected))
    report.data.update({
        "sidx_references": len(expected),
        "moof_count": len(moofs),
        "offset_violations": violations,
        "max_drift_bytes": max_drift,
        "first_violation_index": first_violation,
        "references": entries,
    })
    return report

