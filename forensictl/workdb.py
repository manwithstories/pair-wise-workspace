"""Persistent working database and checkpoint state."""

from __future__ import annotations

import json
import sqlite3
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path

from .events import TimelineEvent
from .sqlitecarve import CarvedRow, write_carved_database
from .timeline import MissingRange
from .verify import VerifyResult


SCHEMA = """
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS hash_mismatches(
    block INTEGER PRIMARY KEY,
    offset INTEGER NOT NULL,
    asset TEXT NOT NULL,
    logical_offset INTEGER NOT NULL,
    expected_sha256 TEXT NOT NULL,
    actual_sha256 TEXT NOT NULL,
    reason TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS verify_issues(
    ordinal INTEGER PRIMARY KEY,
    kind TEXT NOT NULL,
    text TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS carved_events(
    rowid INTEGER PRIMARY KEY,
    entity_key TEXT NOT NULL,
    event_type TEXT NOT NULL,
    description TEXT NOT NULL,
    wall_time TEXT NOT NULL,
    utc_time TEXT,
    tz_offset TEXT,
    source TEXT NOT NULL,
    sequence INTEGER NOT NULL,
    payload TEXT NOT NULL,
    origins TEXT NOT NULL,
    recovered INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS event_rows(
    ordinal INTEGER PRIMARY KEY,
    event_id TEXT NOT NULL UNIQUE,
    utc TEXT NOT NULL,
    entity_key TEXT NOT NULL,
    event_type TEXT NOT NULL,
    description TEXT NOT NULL,
    sources TEXT NOT NULL,
    source_priority INTEGER NOT NULL,
    sequence INTEGER NOT NULL,
    anomalous INTEGER NOT NULL,
    recovered INTEGER NOT NULL,
    raw_json TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS missing_ranges(
    ordinal INTEGER PRIMARY KEY,
    start_utc TEXT NOT NULL,
    end_utc TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS warnings(
    ordinal INTEGER PRIMARY KEY,
    text TEXT NOT NULL
);
"""


@dataclass
class StoredReport:
    events: int
    mismatches: list[tuple[int, int, str, int, str, str, str]]
    missing: list[tuple[datetime, datetime]]
    issues: list[tuple[str, str]]
    warnings: list[str]
    stats: dict
    exit_code: int


class WorkStore:
    def __init__(self, path: str | Path) -> None:
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.connection = sqlite3.connect(self.path)
        self.connection.executescript(SCHEMA)
        self.connection.commit()

    def close(self) -> None:
        self.connection.close()

    def __enter__(self) -> "WorkStore":
        return self

    def __exit__(self, exc_type, exc, tb) -> None:
        self.close()

    def get_meta(self, key: str, default: str | None = None) -> str | None:
        row = self.connection.execute("SELECT value FROM meta WHERE key=?", (key,)).fetchone()
        return row[0] if row else default

    def set_meta(self, key: str, value: str) -> None:
        self.connection.execute(
            "INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value",
            (key, value),
        )
        self.connection.commit()

    def reset_for_run(self, fingerprint: str, bucket_seconds: int) -> None:
        old_fingerprint = self.get_meta("fingerprint")
        old_bucket = self.get_meta("bucket")
        if old_fingerprint != fingerprint or old_bucket != str(bucket_seconds):
            for table in (
                "hash_mismatches",
                "verify_issues",
                "carved_events",
                "event_rows",
                "missing_ranges",
                "warnings",
            ):
                self.connection.execute(f"DELETE FROM {table}")
            self.connection.execute("DELETE FROM meta")
            self.connection.commit()
            self.set_meta("fingerprint", fingerprint)
            self.set_meta("bucket", str(bucket_seconds))
            self.set_meta("stage", "new")

    def stage(self) -> str:
        return self.get_meta("stage", "new") or "new"

    def set_stage(self, stage: str) -> None:
        self.set_meta("stage", stage)

    def store_verify(self, result: VerifyResult) -> None:
        self.connection.execute("DELETE FROM hash_mismatches")
        self.connection.execute("DELETE FROM verify_issues")
        self.connection.executemany(
            "INSERT INTO hash_mismatches VALUES(?,?,?,?,?,?,?)",
            [
                (
                    item.block,
                    item.offset,
                    item.asset,
                    item.logical_offset,
                    item.expected_sha256,
                    item.actual_sha256,
                    item.reason,
                )
                for item in result.mismatches
            ],
        )
        issues = []
        for text in result.structural_errors:
            issues.append(("integrity", text))
        for text in result.input_errors:
            issues.append(("input", text))
        self.connection.executemany(
            "INSERT INTO verify_issues(ordinal,kind,text) VALUES(?,?,?)",
            [(index, kind, text) for index, (kind, text) in enumerate(issues)],
        )
        self.set_meta("expected_blocks", str(result.expected_blocks))
        self.set_meta("manifest_blocks", str(result.manifest_blocks))
        self.set_meta("checked_blocks", str(result.checked_blocks))
        self.set_meta("verify_exit", str(result.exit_code))
        self.connection.commit()

    def store_carved(self, rows: list[CarvedRow], stats: dict) -> None:
        from .sqlitecarve import CarveResult

        self.connection.execute("DELETE FROM carved_events")
        self.connection.commit()
        write_carved_database(
            CarveResult(rows=rows, **{key: value for key, value in stats.items() if key in {
                "warnings", "freelist_pages", "wal_committed_frames",
                "wal_discarded_frames", "live_rows", "recovered_rows"
            }}),
            self.path,
        )
        for key, value in stats.items():
            if isinstance(value, (str, int, float, bool)):
                self.set_meta(f"carve_{key}", str(value))
        self.set_meta(
            "carve_warnings",
            json.dumps(stats.get("warnings", []), sort_keys=True, ensure_ascii=False),
        )
        self.connection.commit()

    def store_timeline(
        self,
        events: list[TimelineEvent],
        missing: list[MissingRange],
        warnings: list[str],
    ) -> None:
        self.connection.execute("DELETE FROM event_rows")
        self.connection.execute("DELETE FROM missing_ranges")
        self.connection.execute("DELETE FROM warnings")
        self.connection.executemany(
            "INSERT INTO event_rows VALUES(?,?,?,?,?,?,?,?,?,?,?,?)",
            [
                (
                    ordinal,
                    event.event_id,
                    event.utc.isoformat(timespec="microseconds"),
                    event.entity_key,
                    event.event_type,
                    event.description,
                    json.dumps(list(event.sources), sort_keys=True),
                    event.source_priority,
                    event.sequence,
                    int(event.anomalous),
                    int(event.recovered),
                    json.dumps(event.raw, sort_keys=True, ensure_ascii=False),
                )
                for ordinal, event in enumerate(events)
            ],
        )
        self.connection.executemany(
            "INSERT INTO missing_ranges VALUES(?,?,?)",
            [
                (ordinal, item.start.isoformat(timespec="microseconds"), item.end.isoformat(timespec="microseconds"))
                for ordinal, item in enumerate(missing)
            ],
        )
        self.connection.executemany(
            "INSERT INTO warnings VALUES(?,?)",
            [(ordinal, text) for ordinal, text in enumerate(warnings)],
        )
        self.set_meta("event_count", str(len(events)))
        self.set_meta("missing_count", str(len(missing)))
        self.connection.commit()

    def report(self) -> StoredReport:
        events = int(self.connection.execute("SELECT COUNT(*) FROM event_rows").fetchone()[0])
        mismatches = self.connection.execute(
            "SELECT block,offset,asset,logical_offset,expected_sha256,actual_sha256,reason "
            "FROM hash_mismatches ORDER BY block"
        ).fetchall()
        missing_rows = self.connection.execute(
            "SELECT start_utc,end_utc FROM missing_ranges ORDER BY ordinal"
        ).fetchall()
        missing = [
            (
                datetime.fromisoformat(start),
                datetime.fromisoformat(end),
            )
            for start, end in missing_rows
        ]
        issues = self.connection.execute(
            "SELECT kind,text FROM verify_issues ORDER BY ordinal"
        ).fetchall()
        warnings = [
            row[0]
            for row in self.connection.execute("SELECT text FROM warnings ORDER BY ordinal")
        ]
        stats = {}
        for key, value in self.connection.execute(
            "SELECT key,value FROM meta ORDER BY key"
        ):
            stats[key] = value
        exit_code = int(self.get_meta("verify_exit", "2") or "2")
        return StoredReport(
            events=events,
            mismatches=list(mismatches),
            missing=missing,
            issues=list(issues),
            warnings=warnings,
            stats=stats,
            exit_code=exit_code,
        )
