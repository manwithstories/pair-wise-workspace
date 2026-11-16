#!/usr/bin/env python3
"""End-to-end demo: generate fixtures, detect, persist, export, verify.

Usage (from the repository root):

    python scripts/run_demo.py --db demo.sqlite --out demo.jsonl

Fixtures are written to ``--work-dir`` (default ``demo_data``) and kept, so
the follow-up batch command in the acceptance notes can read them.  Nothing
outside the given paths is created, and no real user audio is read.
"""

from __future__ import annotations

import argparse
import os
import sqlite3
import sys

REPO_ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
SRC_ROOT = os.path.join(REPO_ROOT, "src")
if SRC_ROOT not in sys.path:
    sys.path.insert(0, SRC_ROOT)

from audio_segmenter.exporter import export_all  # noqa: E402
from audio_segmenter.fixtures import generate_fixtures  # noqa: E402
from audio_segmenter.model import DetectionConfig  # noqa: E402
from audio_segmenter.pipeline import (  # noqa: E402
    discover_wavs,
    execute_batch,
    run_batch,
)
from audio_segmenter.store import Store  # noqa: E402
from audio_segmenter.verify import verify_analyses  # noqa: E402
from audio_segmenter.wavio import WavReader  # noqa: E402

EXPECTED_FILES = 3
SEGMENT_RANGE = (6, 12)
SILENCE_RANGE = (2.0, 8.0)


class DemoFailure(RuntimeError):
    pass


def _check(condition: bool, message: str) -> None:
    if not condition:
        raise DemoFailure(message)


def _count_streamed_frames(path: str) -> int:
    """Decode a fixture block by block; proves the streaming path works."""

    reader = WavReader(path)
    total = 0
    for block in reader.iter_blocks(block_size=4096):
        total += int(block.shape[0])
    return total


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--db", default="demo.sqlite")
    parser.add_argument("--out", default="demo.jsonl")
    parser.add_argument("--csv", default=None)
    parser.add_argument(
        "--work-dir",
        "--data-dir",
        dest="work_dir",
        default="demo_data",
        help="where fixtures are written (kept for the follow-up batch command)",
    )
    parser.add_argument("--seed", type=int, default=20240617)
    args = parser.parse_args(argv)

    data_dir = os.path.abspath(args.work_dir)
    config = DetectionConfig()

    fixture_paths = generate_fixtures(data_dir, seed=args.seed)
    print(f"GENERATED_FIXTURES={len(fixture_paths)}")
    for path in fixture_paths:
        print(f"FIXTURE={os.path.basename(path)} DIFFICULTY=ok")
        frames = _count_streamed_frames(path)
        _check(frames > 0, f"fixture {path} decoded to zero frames")

    paths = discover_wavs(data_dir)
    print(f"FILES={len(paths)}")
    _check(len(paths) == EXPECTED_FILES, "unexpected fixture count")

    analyses = run_batch(paths, config)
    ok = [a for a in analyses if a.status == "ok"]
    _check(len(ok) == len(paths), "some fixtures failed to analyse")

    total_segments = sum(len(a.result.segments) for a in ok)
    total_silence = sum(a.result.total_silence_seconds for a in ok)
    print(f"TOTAL_SEGMENTS={total_segments}")
    print(f"TOTAL_SILENCE_SECONDS={total_silence:.3f}")
    _check(
        SEGMENT_RANGE[0] <= total_segments <= SEGMENT_RANGE[1],
        f"segment count {total_segments} outside {SEGMENT_RANGE}",
    )
    _check(
        SILENCE_RANGE[0] <= total_silence <= SILENCE_RANGE[1],
        f"silence total {total_silence:.3f}s outside {SILENCE_RANGE}",
    )

    global_index = 0
    for analysis in ok:
        result = analysis.result
        assert result is not None
        print(
            f"FILE {os.path.basename(analysis.path)}"
            f" SAMPLE_RATE={result.sample_rate}"
            f" SEGMENTS={len(result.segments)}"
            f" NOISE_FLOOR_DB={result.noise_floor_db:.1f}"
            f" ENTER_DB={result.enter_threshold_db:.1f}"
        )
        for seg in result.segments:
            print(
                f"SEGMENT index={global_index}"
                f" start_sample={seg.start_sample}"
                f" end_sample={seg.end_sample}"
                f" duration={seg.duration_seconds:.3f}"
                f" peak={seg.peak:.4f}"
                f" rms={seg.rms:.4f}"
            )
            global_index += 1
        print(f"SILENCE_SECONDS={result.total_silence_seconds:.3f}")

    db_path = args.db
    if os.path.exists(db_path):
        os.remove(db_path)
    with Store(db_path) as store:
        report = execute_batch(data_dir, config, store=store)
        _check(report.db_error is None, f"database error: {report.db_error}")
        stored = store.stats()
        print(f"DB_FILES={stored['files']} DB_SEGMENTS={stored['segments']}")

    exported = export_all(
        ok, jsonl_path=args.out, csv_path=args.csv
    )
    print(f"EXPORT_RECORDS={exported.records_written} JSONL={args.out}")
    if args.csv:
        print(f"CSV={args.csv}")

    verification = verify_analyses(ok)
    for issue in verification.issues:
        print(f"VERIFY_FAIL file={issue.path} reason={issue.reason}")
    db_ok = _verify_db(db_path)
    print(f"VERIFY={'PASS' if verification.passed and db_ok else 'FAIL'}")
    _check(verification.passed and db_ok, "verification failed")
    print(f"DATA_DIR={data_dir}")
    print(f"DB={db_path}")
    return 0


def _verify_db(db_path: str) -> bool:
    """Independently re-check the persisted rows: conservation + ordering."""

    conn = sqlite3.connect(db_path)
    conn.row_factory = sqlite3.Row
    try:
        rows = conn.execute("SELECT id, total_samples FROM files WHERE status='ok'")
        rows = rows.fetchall()
        if not rows:
            return False
        for row in rows:
            segs = conn.execute(
                "SELECT start_sample, end_sample, sample_count FROM segments"
                " WHERE file_id=? ORDER BY idx",
                (row["id"],),
            ).fetchall()
            sils = conn.execute(
                "SELECT sample_count FROM silences WHERE file_id=?",
                (row["id"],),
            ).fetchall()
            covered = sum(s["sample_count"] for s in segs) + sum(
                s["sample_count"] for s in sils
            )
            if covered != int(row["total_samples"] or 0):
                return False
            cursor = 0
            for seg in segs:
                if seg["start_sample"] < cursor or seg["end_sample"] <= seg["start_sample"]:
                    return False
                cursor = seg["end_sample"]
        return True
    finally:
        conn.close()


if __name__ == "__main__":
    try:
        sys.exit(main())
    except DemoFailure as exc:
        print(f"DEMO_FAILED: {exc}", file=sys.stderr)
        sys.exit(4)
