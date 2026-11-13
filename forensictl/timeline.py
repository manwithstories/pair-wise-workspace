"""Merge forensic events, deduplicate sources, and scan empty time buckets."""

from __future__ import annotations

import math
from dataclasses import dataclass, replace
from datetime import datetime, timedelta, timezone

from .events import SOURCE_PRIORITY, TimelineEvent
from .filemeta import FileMetaResult
from .sqlitecarve import CarvedRow
from .timebase import TimeBase, TimebaseError


MAX_ANOMALOUS_INTERVAL_SECONDS = 600
DEDUP_WINDOW_SECONDS = 2.0


class TimelineError(ValueError):
    """Raised when timeline construction cannot proceed."""


@dataclass(frozen=True)
class MissingRange:
    start: datetime
    end: datetime


@dataclass
class TimelineResult:
    events: list[TimelineEvent]
    missing_ranges: list[MissingRange]
    warnings: list[str]


def _event_from_carved(row: CarvedRow, timebase: TimeBase) -> TimelineEvent:
    try:
        if row.utc_time:
            normalized = timebase.normalize_utc(row.utc_time)
            original = row.utc_time
        else:
            wall = row.wall_time
            if row.tz_offset and not (
                wall.endswith("Z")
                or wall.endswith("z")
                or len(wall) >= 6
                and wall[-6] in "+-"
                and wall[-3] == ":"
            ):
                wall = f"{wall}{row.tz_offset}"
            normalized = timebase.normalize_local(wall)
            original = wall
    except TimebaseError as exc:
        raise TimelineError(f"SQLite event {row.rowid} has an invalid time: {exc}") from exc
    raw = {
        "rowid": row.rowid,
        "original_time": original,
        "wall_time": row.wall_time,
        "utc_time": row.utc_time,
        "tz_offset": row.tz_offset,
        "normalized_utc": normalized.utc.isoformat(timespec="microseconds"),
        "clock_correction_seconds": normalized.correction_seconds,
        "dst_kind": normalized.dst_kind,
        "origins": list(row.origins),
        "recovered": row.recovered,
    }
    return TimelineEvent(
        event_id=f"sqlite:{row.rowid}",
        utc=normalized.utc,
        entity_key=row.entity_key,
        event_type=row.event_type,
        description=row.description,
        sources=("sqlite",),
        source_priority=SOURCE_PRIORITY["sqlite"],
        sequence=row.sequence,
        raw=raw,
        anomalous=normalized.anomalous,
        recovered=row.recovered,
    )


def _can_merge(left: TimelineEvent, right: TimelineEvent) -> bool:
    if left.entity_key != right.entity_key:
        return False
    if left.event_type == "METADATA_ANOMALY" or right.event_type == "METADATA_ANOMALY":
        return False
    if set(left.sources) == set(right.sources):
        return False
    return abs((left.utc - right.utc).total_seconds()) <= DEDUP_WINDOW_SECONDS


def merge_events(events: list[TimelineEvent]) -> list[TimelineEvent]:
    grouped: dict[str, list[TimelineEvent]] = {}
    for event in events:
        grouped.setdefault(event.entity_key, []).append(event)
    merged: list[TimelineEvent] = []
    for entity_key in sorted(grouped):
        entity_events = sorted(grouped[entity_key], key=lambda item: item.sort_key())
        current: list[TimelineEvent] = []
        for event in entity_events:
            if current and _can_merge(current[-1], event):
                current[-1] = current[-1].merged_with(event)
            else:
                current.append(event)
        merged.extend(current)
    return sorted(merged, key=lambda item: item.sort_key())


def enforce_monotonic(
    events: list[TimelineEvent],
    threshold_seconds: int = MAX_ANOMALOUS_INTERVAL_SECONDS,
) -> list[TimelineEvent]:
    ordered = sorted(events, key=lambda item: item.sort_key())
    result: list[TimelineEvent] = []
    previous: TimelineEvent | None = None
    for event in ordered:
        adjusted = event
        if previous is not None:
            interval = (adjusted.utc - previous.utc).total_seconds()
            if interval < 0:
                raw = dict(adjusted.raw)
                raw["monotonic_adjustment_seconds"] = -interval
                adjusted = replace(
                    adjusted,
                    utc=previous.utc,
                    anomalous=True,
                    raw=raw,
                )
            elif interval > threshold_seconds and not adjusted.anomalous:
                adjusted = replace(adjusted, anomalous=True)
        result.append(adjusted)
        previous = adjusted
    return sorted(result, key=lambda item: item.sort_key())


def _subtract_exclusions(
    start: datetime,
    end: datetime,
    exclusions: list[tuple[datetime, datetime]],
) -> list[MissingRange]:
    remaining = [(start, end)]
    for exclusion_start, exclusion_end in sorted(exclusions):
        next_remaining = []
        for item_start, item_end in remaining:
            if exclusion_end <= item_start or exclusion_start >= item_end:
                next_remaining.append((item_start, item_end))
                continue
            if exclusion_start > item_start:
                next_remaining.append((item_start, exclusion_start))
            if exclusion_end < item_end:
                next_remaining.append((exclusion_end, item_end))
        remaining = next_remaining
    return [
        MissingRange(start=item_start, end=item_end)
        for item_start, item_end in remaining
        if item_end > item_start
    ]


def scan_missing_ranges(
    events: list[TimelineEvent],
    bucket_seconds: int = 300,
    exclusions: list[tuple[datetime, datetime]] | None = None,
) -> list[MissingRange]:
    if bucket_seconds <= 0:
        raise TimelineError("bucket size must be positive")
    if not events:
        return []
    exclusions = exclusions or []
    epoch = datetime(1970, 1, 1, tzinfo=timezone.utc)

    def bucket_start(value: datetime) -> datetime:
        seconds = math.floor(value.timestamp())
        return epoch + timedelta(seconds=(seconds // bucket_seconds) * bucket_seconds)

    first = bucket_start(min(event.utc for event in events))
    last = bucket_start(max(event.utc for event in events))
    occupied = {bucket_start(event.utc) for event in events}
    empty_runs: list[tuple[datetime, datetime]] = []
    cursor = first
    run_start: datetime | None = None
    while cursor <= last:
        if cursor in occupied:
            if run_start is not None:
                empty_runs.append((run_start, cursor))
                run_start = None
        elif run_start is None:
            run_start = cursor
        cursor += timedelta(seconds=bucket_seconds)
    if run_start is not None:
        empty_runs.append((run_start, last + timedelta(seconds=bucket_seconds)))

    missing: list[MissingRange] = []
    for start, end in empty_runs:
        missing.extend(_subtract_exclusions(start, end, exclusions))
    return sorted(missing, key=lambda item: (item.start, item.end))


def build_timeline(
    carved_rows: list[CarvedRow],
    filemeta: FileMetaResult,
    timebase: TimeBase,
    *,
    bucket_seconds: int = 300,
    maintenance_windows: list[tuple[datetime, datetime]] | None = None,
) -> TimelineResult:
    events = [_event_from_carved(row, timebase) for row in carved_rows]
    events.extend(filemeta.events)
    merged = merge_events(events)
    ordered = enforce_monotonic(merged)
    exclusions = list(maintenance_windows or [])
    exclusions.extend(timebase.timezone_gap_windows(padding=timedelta(minutes=30)))
    gaps = scan_missing_ranges(ordered, bucket_seconds, exclusions)
    warnings = list(filemeta.warnings)
    return TimelineResult(events=ordered, missing_ranges=gaps, warnings=warnings)
