from __future__ import annotations

from datetime import datetime
from pathlib import Path

from forensictl.filemeta import parse_filemeta
from forensictl.imageio import ImageIO
from forensictl.sqlitecarve import carve_sqlite
from forensictl.timebase import TimeBase, load_maintenance_windows
from forensictl.timeline import build_timeline


def _timeline(image_path: Path):
    with ImageIO.open(image_path) as image:
        carved = carve_sqlite(image)
        filemeta = parse_filemeta(image)
        timebase = TimeBase.from_image(image)
        maintenance = load_maintenance_windows(image)
    result = build_timeline(
        carved.rows,
        filemeta,
        timebase,
        bucket_seconds=300,
        maintenance_windows=maintenance,
    )
    return result, filemeta


def test_timeline_counts_merge_and_monotonicity(image_path: Path) -> None:
    result, filemeta = _timeline(image_path)
    assert len(result.events) == 1284
    assert len(result.missing_ranges) == 2
    assert len(filemeta.records) == 300
    assert len(filemeta.anomalies) == 4
    merged = [event for event in result.events if len(set(event.sources)) > 1]
    assert len(merged) == 20
    assert all(set(event.sources) == {"file_record", "sqlite"} for event in merged)
    ordered = sorted(result.events, key=lambda event: event.sort_key())
    assert all(
        (ordered[index + 1].utc - ordered[index].utc).total_seconds() >= 0
        for index in range(len(ordered) - 1)
    )
    assert any(event.anomalous for event in result.events)


def test_anomaly_events_keep_original_values(image_path: Path) -> None:
    result, _ = _timeline(image_path)
    anomalies = [event for event in result.events if event.event_type == "METADATA_ANOMALY"]
    assert len(anomalies) == 4
    acquired = datetime.fromisoformat("2026-03-08T22:31:00+00:00")
    assert any(
        datetime.fromisoformat(event.raw["modified_utc"]) > acquired
        for event in anomalies
    )
    assert any(
        event.raw["modified_utc_100ns"] < event.raw["created_utc_100ns"]
        for event in anomalies
    )


def test_missing_ranges_exclude_dst_and_maintenance(image_path: Path) -> None:
    result, _ = _timeline(image_path)
    starts = [item.start for item in result.missing_ranges]
    assert any(item.hour == 0 for item in starts)
    assert any(item.hour == 9 for item in starts)
    for item in result.missing_ranges:
        assert not (item.start < datetime.fromisoformat("2026-03-08T17:00:00+00:00") and item.end > datetime.fromisoformat("2026-03-08T14:00:00+00:00"))
        assert not (item.start < datetime.fromisoformat("2026-03-08T07:30:00+00:00") and item.end > datetime.fromisoformat("2026-03-08T06:30:00+00:00"))
