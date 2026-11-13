from __future__ import annotations

import sqlite3
from pathlib import Path

from forensictl.imageio import ImageIO
from forensictl.sqlitecarve import carve_sqlite


def test_freelist_wal_and_overflow_recovery(image_path: Path) -> None:
    with ImageIO.open(image_path) as image:
        result = carve_sqlite(image)
    assert len(result.rows) == 700
    assert result.live_rows == 696
    assert result.recovered_rows == 4
    assert result.wal_committed_frames == 3
    assert result.wal_discarded_frames >= 1
    rowids = [row.rowid for row in result.rows]
    assert rowids == sorted(set(rowids))
    assert not any(row.rowid >= 9000 for row in result.rows)

    by_id = {row.rowid: row for row in result.rows}
    assert "freelist" in by_id[7].origins
    assert "freelist" in by_id[99].origins
    assert by_id[7].recovered and by_id[99].recovered
    assert by_id[233].recovered and by_id[456].recovered
    assert any(origin.startswith("wal") for origin in by_id[233].origins)
    assert len(by_id[333].payload) > 6000
    assert not result.warnings


def test_carver_does_not_open_source_with_sqlite3(image_path: Path, monkeypatch) -> None:
    def forbidden(*args, **kwargs):
        raise AssertionError("sqlite3 must not open the evidence image")

    monkeypatch.setattr(sqlite3, "connect", forbidden)
    with ImageIO.open(image_path) as image:
        result = carve_sqlite(image)
    assert len(result.rows) == 700
