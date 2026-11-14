from __future__ import annotations

import hashlib
import os
import sqlite3
from pathlib import Path

from .checks import CheckReport, CheckedAsset
from .errors import EXIT_IO, IndexWriteError, MediaError


SCHEMA = """
CREATE TABLE IF NOT EXISTS assets (
  sha256 TEXT PRIMARY KEY,
  path TEXT NOT NULL,
  container TEXT NOT NULL,
  format TEXT NOT NULL,
  size_bytes INTEGER NOT NULL,
  duration_ms INTEGER NOT NULL,
  track_count INTEGER NOT NULL,
  status TEXT NOT NULL,
  issue_count INTEGER NOT NULL,
  blocking_count INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS tracks (
  sha256 TEXT NOT NULL,
  ordinal INTEGER NOT NULL,
  track_id INTEGER NOT NULL,
  handler TEXT NOT NULL,
  codec TEXT NOT NULL,
  duration_ms INTEGER NOT NULL,
  profile TEXT,
  level TEXT,
  sample_rate INTEGER,
  channels INTEGER,
  PRIMARY KEY (sha256, ordinal)
);
CREATE TABLE IF NOT EXISTS issues (
  sha256 TEXT NOT NULL,
  issue_key TEXT NOT NULL,
  code TEXT NOT NULL,
  severity TEXT NOT NULL,
  track_id INTEGER,
  delta_ms INTEGER,
  limit_ms INTEGER,
  message TEXT NOT NULL,
  box_path TEXT NOT NULL,
  offset INTEGER NOT NULL,
  repair_advice TEXT NOT NULL,
  PRIMARY KEY (sha256, issue_key)
);
CREATE TABLE IF NOT EXISTS runs (
  run_key TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  bundle_path TEXT NOT NULL,
  manifest_sha256 TEXT NOT NULL,
  asset_count INTEGER NOT NULL,
  passed INTEGER NOT NULL,
  rejected INTEGER NOT NULL,
  track_count INTEGER NOT NULL,
  tracks_written INTEGER NOT NULL
);
"""
TABLE_NAMES = ("assets", "tracks", "issues", "runs")


def _issue_key(index: int, issue: dict) -> str:
    return (
        f"{index:04d}|{issue['code']}|{issue.get('track_id')}|"
        f"{issue.get('box_path', '')}|{issue.get('delta_ms')}"
    )


def _fallback_sha(path: str) -> str:
    return "path:" + hashlib.sha256(path.encode("utf-8")).hexdigest()


class IndexDB:
    def __init__(self, path: str | os.PathLike[str]) -> None:
        self.path = Path(path)
        try:
            self.path.parent.mkdir(parents=True, exist_ok=True)
            self.connection = sqlite3.connect(os.fspath(self.path), isolation_level="DEFERRED")
            self.connection.executescript(SCHEMA)
            self.connection.commit()
        except (OSError, sqlite3.Error) as exc:
            raise IndexWriteError(f"cannot open index: {exc}", path=os.fspath(self.path)) from exc

    def close(self) -> None:
        self.connection.close()

    def __enter__(self) -> "IndexDB":
        return self

    def __exit__(self, exc_type, exc, traceback) -> None:
        self.close()

    def _upsert_asset(self, checked: CheckedAsset) -> None:
        asset = checked.asset
        sha256 = asset.sha256 if asset else _fallback_sha(checked.path)
        self.connection.execute(
            """
            INSERT INTO assets VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
            ON CONFLICT(sha256) DO UPDATE SET
              path=excluded.path, container=excluded.container, format=excluded.format,
              size_bytes=excluded.size_bytes, duration_ms=excluded.duration_ms,
              track_count=excluded.track_count, status=excluded.status,
              issue_count=excluded.issue_count, blocking_count=excluded.blocking_count
            """,
            (
                sha256,
                checked.path,
                asset.container if asset else "unknown",
                asset.format if asset else "unknown",
                asset.size_bytes if asset else 0,
                asset.duration_ms if asset else 0,
                asset.track_count if asset else 0,
                checked.status,
                len(checked.issues),
                checked.blocking_count,
            ),
        )
        if asset is not None:
            for ordinal, track in enumerate(asset.tracks):
                self.connection.execute(
                    """
                    INSERT INTO tracks VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                    ON CONFLICT(sha256, ordinal) DO UPDATE SET
                      track_id=excluded.track_id, handler=excluded.handler, codec=excluded.codec,
                      duration_ms=excluded.duration_ms, profile=excluded.profile, level=excluded.level,
                      sample_rate=excluded.sample_rate, channels=excluded.channels
                    """,
                    (
                        sha256,
                        ordinal,
                        track.track_id,
                        track.handler,
                        track.codec,
                        track.duration_ms,
                        track.profile,
                        track.level,
                        track.sample_rate,
                        track.channels,
                    ),
                )
        for issue_index, issue in enumerate(checked.issues):
            self.connection.execute(
                """
                INSERT INTO issues VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(sha256, issue_key) DO UPDATE SET
                  code=excluded.code, severity=excluded.severity, track_id=excluded.track_id,
                  delta_ms=excluded.delta_ms, limit_ms=excluded.limit_ms, message=excluded.message,
                  box_path=excluded.box_path, offset=excluded.offset, repair_advice=excluded.repair_advice
                """,
                (
                    sha256,
                    _issue_key(issue_index, issue),
                    issue["code"],
                    issue["severity"],
                    issue.get("track_id"),
                    issue.get("delta_ms"),
                    issue.get("limit_ms"),
                    issue["message"],
                    issue.get("box_path", ""),
                    issue.get("offset", 0),
                    issue.get("repair_advice", ""),
                ),
            )

    def ingest_report(self, report: CheckReport, run_info: dict | None = None) -> None:
        try:
            with self.connection:
                for checked in report.assets:
                    self._upsert_asset(checked)
                if run_info is not None:
                    self.connection.execute(
                        """
                        INSERT INTO runs VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
                        ON CONFLICT(run_key) DO UPDATE SET
                          kind=excluded.kind, bundle_path=excluded.bundle_path,
                          manifest_sha256=excluded.manifest_sha256, asset_count=excluded.asset_count,
                          passed=excluded.passed, rejected=excluded.rejected,
                          track_count=excluded.track_count, tracks_written=excluded.tracks_written
                        """,
                        (
                            run_info["run_key"],
                            run_info["kind"],
                            run_info["bundle_path"],
                            run_info["manifest_sha256"],
                            run_info["asset_count"],
                            run_info["passed"],
                            run_info["rejected"],
                            run_info["track_count"],
                            run_info["tracks_written"],
                        ),
                    )
        except (sqlite3.Error, OSError, KeyError) as exc:
            raise IndexWriteError(f"index transaction rolled back: {exc}", path=os.fspath(self.path)) from exc

    ingest = ingest_report

    def counts(self) -> dict[str, int]:
        return {
            name: int(self.connection.execute(f"SELECT COUNT(*) FROM {name}").fetchone()[0])
            for name in TABLE_NAMES
        }

    def query_issues(self, severity: str | None = None) -> list[dict]:
        sql = """
          SELECT a.path, i.severity, i.code, i.track_id, i.delta_ms, i.limit_ms,
                 i.message, i.box_path, i.offset, i.repair_advice
          FROM issues i JOIN assets a USING(sha256)
        """
        parameters: tuple = ()
        if severity:
            sql += " WHERE i.severity = ?"
            parameters = (severity,)
        sql += " ORDER BY a.path, i.issue_key"
        try:
            rows = self.connection.execute(sql, parameters).fetchall()
        except sqlite3.Error as exc:
            raise MediaError("INDEX_READ_FAILED", str(exc), path=os.fspath(self.path), exit_code=EXIT_IO) from exc
        keys = [
            "asset",
            "severity",
            "code",
            "track_id",
            "delta_ms",
            "limit_ms",
            "message",
            "box_path",
            "offset",
            "repair_advice",
        ]
        return [dict(zip(keys, row)) for row in rows]
