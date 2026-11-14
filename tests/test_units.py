"""Unit tests: reorder window, repair edge cases, strict mode, stream pick."""

from __future__ import annotations

import numpy as np
import pytest

from plateqc import cli, repair, reorder
from plateqc.decoder import FrameRecord
from plateqc.fixtures import make_samples


@pytest.fixture(scope="session")
def samples(tmp_path_factory):
    out_dir = tmp_path_factory.mktemp("samples")
    make_samples.main(["--out", str(out_dir)])
    return out_dir


def _records(pts_list):
    return [
        FrameRecord(
            decode_index=i,
            pts=pts,
            dts=None if pts is None else pts - 1000,
            pict_type="P",
            key_frame=False,
        )
        for i, pts in enumerate(pts_list)
    ]


def test_reorder_b_frames_roundtrip():
    # Decode order of a BBP GOP: I0 P3 B1 B2 P6 B4 B5 ...
    pts_decode_order = [0, 3000, 1000, 2000, 6000, 4000, 5000, 9000, 7000, 8000]
    ordered, warnings = reorder.reorder(_records(pts_decode_order), 1000)
    assert warnings == []
    assert [r.pts for r in ordered] == sorted(pts_decode_order)


def test_reorder_overflow_falls_back_to_dts():
    # 40 frames fully reversed: deeper than the capacity-16 window.
    pts_decode_order = [(39 - i) * 1000 for i in range(40)]
    records = _records(pts_decode_order)
    for i, record in enumerate(records):
        record.dts = i * 1000  # DTS stays monotonic in decode order
    ordered, warnings = reorder.reorder(records, 1000)
    assert warnings == [reorder.REORDER_OVERFLOW]
    # Fallback is (inferred) DTS order == decode order here.
    assert [r.decode_index for r in ordered] == list(range(40))


def test_reorder_missing_dts_inferred():
    records = _records([2000, 0, 1000])
    for record in records:
        record.dts = None
    ordered, warnings = reorder.reorder(records, 1000)
    assert warnings == []
    assert [r.pts for r in ordered] == [0, 1000, 2000]


def test_repair_all_timestamps_missing():
    pts = np.full(10, np.nan)
    warnings = []
    repaired, mask = repair._repair_pts(pts, 1000, warnings)
    assert mask.all()
    assert repaired.tolist() == [i * 1000 for i in range(10)]
    assert warnings == [repair.WARN_MISSING]


def test_repair_clean_grid_untouched():
    pts = np.arange(5, dtype=np.float64) * 1000
    warnings = []
    repaired, mask = repair._repair_pts(pts, 1000, warnings)
    assert not mask.any()
    assert repaired.tolist() == [0, 1000, 2000, 3000, 4000]
    assert warnings == []


def test_strict_reorder_overflow_exit_4(samples, monkeypatch):
    original = reorder.reorder

    def fake(records, interval, capacity=16):
        ordered, _ = original(records, interval, capacity)
        return ordered, [reorder.REORDER_OVERFLOW]

    monkeypatch.setattr(cli.reorder, "reorder", fake)
    rc = cli.main(
        ["report", "--input", str(samples / "plate_bframes_normal.mp4"), "--strict"]
    )
    assert rc == cli.EXIT_STRICT


def test_strict_normal_input_passes(samples):
    rc = cli.main(
        ["report", "--input", str(samples / "plate_bframes_normal.mp4"), "--strict"]
    )
    assert rc == cli.EXIT_OK


def test_stream_selection(samples):
    ok = cli.main(
        ["report", "--input", str(samples / "plate_all_i.mp4"), "--stream", "0"]
    )
    assert ok == cli.EXIT_OK
    bad = cli.main(
        ["report", "--input", str(samples / "plate_all_i.mp4"), "--stream", "5"]
    )
    assert bad == cli.EXIT_UNDECODABLE


def test_normalize_failure_keeps_existing_output(samples, tmp_path):
    output = tmp_path / "existing.mp4"
    output.write_bytes(b"previous content")
    rc = cli.main(
        [
            "normalize",
            "--input",
            "does_not_exist.mp4",
            "--output",
            str(output),
        ]
    )
    assert rc == cli.EXIT_UNDECODABLE
    assert output.read_bytes() == b"previous content"
