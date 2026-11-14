"""Deterministic end-to-end checks over generated fixtures."""

from __future__ import annotations

import os
import tempfile
from pathlib import Path
from typing import Callable

from mkvtomp4.ebml.reader import EBMLReadError
from mkvtomp4.mkv.blocks import MultiSegmentError, parse_media_index
from mkvtomp4.mkv.tracks import NoUsableTracksError, ZeroSamplesError
from mkvtomp4.mkv.tree import MatroskaTree
from mkvtomp4.mp4.boxes import iter_box_headers
from mkvtomp4.mp4.export_frag import export_fragmented
from mkvtomp4.mp4.export_plain import export_plain
from mkvtomp4.timing.normalize import normalize_media
from mkvtomp4.verify import verify_output


def _run(checks: list[dict[str, object]], name: str, func: Callable[[], object]) -> None:
    try:
        detail = func()
        checks.append({"name": name, "ok": True, "detail": detail})
    except BaseException as exc:
        checks.append(
            {"name": name, "ok": False, "error": f"{type(exc).__name__}: {exc}"}
        )


def _expect(exc_type: type[BaseException], func: Callable[[], object]) -> dict[str, object]:
    try:
        func()
    except exc_type as exc:
        return {"exception": type(exc).__name__, "message": str(exc)}
    raise AssertionError(f"expected {exc_type.__name__}")


def _top_boxes(path: str) -> list[str]:
    with open(path, "rb") as stream:
        return [header.box_type for header in iter_box_headers(stream)]


def run_selftest(fixtures: str) -> dict[str, object]:
    root = Path(fixtures)
    checks: list[dict[str, object]] = []
    if not (root / "live_unknown.mkv").exists():
        return {
            "ok": False,
            "checks": [],
            "passed": 0,
            "failed": 1,
            "error": f"fixtures missing under {fixtures}; run fixtures/gen_fixtures.py first",
        }
    with tempfile.TemporaryDirectory(prefix="mkvtomp4-selftest-") as temp:
        plain = os.path.join(temp, "plain.mp4")
        frag = os.path.join(temp, "frag.mp4")
        normalized = [None]

        def live_probe():
            summary = MatroskaTree(str(root / "live_unknown.mkv")).summary()
            assert 10_000 <= summary.element_count <= 13_000, summary.element_count
            assert summary.max_depth == 6, summary.max_depth
            assert summary.unknown_size_count >= 3, summary.unknown_size_count
            assert summary.segment_count == 1
            assert summary.cue_entries == 0
            assert len(summary.first_elements) == 20
            return summary.element_count, summary.unknown_size_count

        def live_tracks():
            media = parse_media_index(str(root / "live_unknown.mkv"))
            assert len(media.tracks) == 2
            assert len(media.skipped) == 1 and media.skipped[0].track_type == 17
            result = normalize_media(media)
            assert result.negative_shift_ms > 0
            assert result.dropped_dup_dts > 0
            assert result.reordered_pts > 0
            assert result.timestamp_summary()["first_dts"] == 0
            normalized[0] = result
            return result.timestamp_summary()

        def plain_roundtrip():
            assert normalized[0] is not None
            export_plain(normalized[0], plain)
            boxes = _top_boxes(plain)
            assert boxes.index("moov") < boxes.index("mdat"), boxes
            result = verify_output(plain, normalized[0])
            assert result["ok"] and result["sample_delta"] == 0
            return {"boxes": boxes[:3], "timestamp_delta": result["timestamp_delta"]}

        def frag_roundtrip():
            assert normalized[0] is not None
            exported = export_fragmented(normalized[0], frag, frag_duration=2.0)
            assert "moof" in _top_boxes(frag)
            result = verify_output(frag, normalized[0])
            assert result["ok"] and result["sample_delta"] == 0
            return {
                "fragments": exported["fragment_count"],
                "timestamp_delta": result["timestamp_delta"],
            }

        def cue_checks():
            good = parse_media_index(str(root / "with_cues.mkv")).cue_stats
            bad = parse_media_index(str(root / "bad_cues.mkv")).cue_stats
            no_cues = parse_media_index(str(root / "no_cues.mkv")).cue_stats
            assert good.valid == good.entries and good.index_source == "cues"
            assert bad.rejected > 0
            assert no_cues.index_source == "scan" and no_cues.rebuilt_samples > 0
            return {
                "valid": good.valid,
                "rejected": bad.rejected,
                "rebuilt_samples": no_cues.rebuilt_samples,
            }

        _run(checks, "live_unknown_probe", live_probe)
        _run(checks, "timestamp_normalization", live_tracks)
        _run(checks, "plain_roundtrip", plain_roundtrip)
        _run(checks, "frag_roundtrip", frag_roundtrip)
        _run(checks, "cue_rebuild_and_reject", cue_checks)
        _run(
            checks,
            "multi_segment",
            lambda: MatroskaTree(str(root / "multi_segment.mkv")).summary().segment_count,
        )
        _run(
            checks,
            "multi_segment_remux_error",
            lambda: _expect(
                MultiSegmentError,
                lambda: parse_media_index(str(root / "multi_segment.mkv")),
            ),
        )
        _run(
            checks,
            "crc_warning",
            lambda: len(MatroskaTree(str(root / "crc_bad.mkv")).warnings),
        )
        _run(
            checks,
            "truncated_error",
            lambda: _expect(EBMLReadError, lambda: MatroskaTree(str(root / "truncated.mkv"))),
        )
        _run(
            checks,
            "missing_tracks_error",
            lambda: _expect(
                NoUsableTracksError,
                lambda: parse_media_index(str(root / "no_tracks.mkv")),
            ),
        )
        _run(
            checks,
            "zero_samples_error",
            lambda: _expect(
                ZeroSamplesError,
                lambda: parse_media_index(str(root / "zero_samples.mkv")),
            ),
        )
    failed = [check for check in checks if not check["ok"]]
    return {
        "ok": not failed,
        "checks": checks,
        "passed": len(checks) - len(failed),
        "failed": len(failed),
    }
