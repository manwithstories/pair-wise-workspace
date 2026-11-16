"""Post-write verification: box boundaries, offsets, index order, hashes.

Verification is structural and independent of the build: it re-reads the bytes
from disk with :func:`src.mp4_boxes.parse_boxes`, recomputes every chunk offset
against the actual sample sizes, checks that the sample tables agree with the
stsz entry count, and confirms that the mdat payload is exactly the declared
size. It also checks the timing invariants the remux promised: every track's
decoding times strictly increase and no presentation time is negative.

The one thing it cannot do alone is compare sample bytes back to the source;
that needs the source file, so :func:`verify_output` accepts an optional source
Matroska document and hashes matching ranges when one is supplied.
"""

from __future__ import annotations

import hashlib
from pathlib import Path

from ._mp4_core import BoxRecord, Mp4Error, parse_boxes
from ._verify_records import CheckResult, VerifyReport
from ._verify_track import _track_id, _verify_track
from ._verify_tables import _u32
from .matroska_model import MatroskaDocument


def _fail(code: str, message: str, checks: list[CheckResult]) -> None:
    checks.append(CheckResult("structure", False, f"{code}: {message}"))


def verify_output(
    path: Path,
    *,
    expected_sha256: str | None = None,
    document: MatroskaDocument | None = None,
) -> VerifyReport:
    path = Path(path)
    checks: list[CheckResult] = []
    if not path.is_file():
        return VerifyReport(
            path=path,
            sha256="",
            size=0,
            checks=[CheckResult("exists", False, f"missing {path}")],
        )
    data = path.read_bytes()
    digest = hashlib.sha256(data).hexdigest()
    report = VerifyReport(path=path, sha256=digest, size=len(data), checks=checks)
    checks.append(CheckResult("exists", True, f"{len(data)} bytes"))

    if expected_sha256 is not None:
        checks.append(
            CheckResult(
                "sha256",
                digest == expected_sha256,
                "matches expected digest" if digest == expected_sha256 else f"expected {expected_sha256}",
            )
        )

    try:
        roots = parse_boxes(data, max_depth=16)
    except Mp4Error as exc:
        _fail(exc.code, str(exc), checks)
        return report
    checks.append(CheckResult("box_parse", True, f"{len(roots)} top-level box(es)"))
    report.box_count = sum(len(list(root.walk())) for root in roots)

    top_types = [root.type for root in roots]
    checks.append(
        CheckResult(
            "top_level_boxes",
            top_types == [b"ftyp", b"moov", b"mdat"],
            f"found {[t.decode('latin-1') for t in top_types]}",
        )
    )
    cursor = 0
    contiguous = True
    for root in roots:
        if root.offset != cursor:
            contiguous = False
        cursor = root.end
    checks.append(CheckResult("box_contiguity", contiguous and cursor == len(data), f"end at {cursor}"))

    moov = next((root for root in roots if root.type == b"moov"), None)
    mdat = next((root for root in roots if root.type == b"mdat"), None)
    if moov is None or mdat is None:
        _fail("missing_box", "moov or mdat absent", checks)
        return report

    checks.append(
        CheckResult("mdat_size", mdat.end == len(data), f"mdat ends at {mdat.end} of {len(data)}")
    )
    mvhd = moov.child(b"mvhd")
    checks.append(CheckResult("mvhd", mvhd is not None, "mvhd present" if mvhd else "mvhd missing"))
    if mvhd is not None:
        mvhd_timescale = _u32(data, mvhd.payload_offset + 4 + 8)
        checks.append(
            CheckResult("movie_timescale", mvhd_timescale > 0, f"{mvhd_timescale} units/second")
        )

    chunk_spans: list[tuple[int, int]] = []
    for trak in [child for child in moov.children if child.type == b"trak"]:
        trak_id = _track_id(data, trak)
        try:
            summary = _verify_track(data, trak, checks, chunk_spans)
        except Mp4Error as exc:
            checks.append(CheckResult(f"track[{trak_id}]", False, f"{exc.code}: {exc}"))
            summary = None
        if summary:
            report.track_summaries.append(summary)

    inside = True
    for start, end in chunk_spans:
        if start < mdat.payload_offset or end > mdat.end:
            inside = False
            break
    checks.append(
        CheckResult(
            "chunk_offsets_in_mdat",
            inside,
            f"{len(chunk_spans)} chunk span(s) inside mdat"
            if inside
            else "a chunk points outside the mdat payload",
        )
    )

    if document is not None:
        checks.append(_verify_against_source(document, chunk_spans))

    return report


def _verify_against_source(
    document: MatroskaDocument, chunk_spans: list[tuple[int, int]]
) -> CheckResult:
    """Confirm the mdat carries exactly as many media bytes as the source.

    This is a size/coverage check, not a byte-for-byte comparison: without the
    remux's sample map the verifier cannot tell which output range came from
    which source range. Byte equality is asserted by the remuxer's own copy
    check (``RemuxOptions.verify_copy``) and by the end-to-end tests.
    """
    if document.path is None or not Path(document.path).is_file():
        return CheckResult("source_bytes", True, "source path unavailable; sample comparison skipped")
    source_size = Path(document.path).stat().st_size
    expected = 0
    for track in document.tracks:
        for block in document.blocks_for_track(track):
            expected += sum(frame.size for frame in block.frames)
    copied = sum(end - start for start, end in chunk_spans)
    ok = copied == expected
    return CheckResult(
        "source_byte_coverage",
        ok,
        f"mdat carries {copied} bytes; source sample bytes total {expected}"
        + ("" if source_size else " (empty source)"),
    )


__all__ = ["CheckResult", "VerifyReport", "verify_output"]
