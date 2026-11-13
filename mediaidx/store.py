"""SQLite persistence: migrations, WAL, decode cache, LSH buckets and clusters."""

from __future__ import annotations

import itertools
import json
import os
import sqlite3
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterable, Iterator, Sequence

from mediaidx.canonical import MediaFamily, MediaKind
from mediaidx.fingerprint import candidate_hashes, split_hash_bands, unpack_fingerprint


SCHEMA_VERSION = 1


class StoreError(RuntimeError):
    pass


class Store:
    """A single-writer SQLite store.

    Only the indexer's main thread calls mutating methods.  Worker processes
    never receive a connection, which keeps the write path serialized.
    """

    def __init__(self, db_path: str | os.PathLike[str]) -> None:
        self.db_path = str(db_path)
        if self.db_path != ":memory:" and not self.db_path.startswith("file:"):
            Path(self.db_path).expanduser().parent.mkdir(parents=True, exist_ok=True)
        self.connection = sqlite3.connect(self.db_path, timeout=30, isolation_level=None)
        self.connection.row_factory = sqlite3.Row
        self.connection.execute("PRAGMA foreign_keys=ON")
        self.connection.execute("PRAGMA busy_timeout=30000")
        self.connection.execute("PRAGMA journal_mode=WAL")
        self.connection.execute("PRAGMA synchronous=NORMAL")
        self.migrate()

    def close(self) -> None:
        try:
            self.connection.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        except sqlite3.Error:
            pass
        self.connection.close()

    def __enter__(self) -> "Store":
        return self

    def __exit__(self, exc_type: object, exc: object, traceback: object) -> None:
        self.close()

    @contextmanager
    def transaction(self) -> Iterator[sqlite3.Connection]:
        self.connection.execute("BEGIN IMMEDIATE")
        try:
            yield self.connection
        except BaseException:
            self.connection.execute("ROLLBACK")
            raise
        else:
            self.connection.execute("COMMIT")

    def migrate(self) -> None:
        connection = self.connection
        with self.transaction() as cursor:
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS schema_meta (
                    key TEXT PRIMARY KEY,
                    value TEXT NOT NULL
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS files (
                    id INTEGER PRIMARY KEY,
                    rel_path TEXT NOT NULL UNIQUE,
                    family TEXT NOT NULL,
                    format TEXT NOT NULL,
                    content_hash TEXT NOT NULL,
                    size INTEGER NOT NULL,
                    mtime_ns INTEGER NOT NULL,
                    status TEXT NOT NULL,
                    fingerprint BLOB,
                    candidate_hashes TEXT NOT NULL DEFAULT '[]',
                    metadata TEXT NOT NULL DEFAULT '{}',
                    loader_name TEXT,
                    loader_version TEXT,
                    error_type TEXT,
                    error_message TEXT,
                    first_seen REAL NOT NULL,
                    updated_at REAL NOT NULL
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS decode_cache (
                    content_hash TEXT NOT NULL,
                    family TEXT NOT NULL,
                    loader_version TEXT NOT NULL,
                    payload BLOB NOT NULL,
                    candidate_hashes TEXT NOT NULL,
                    metadata TEXT NOT NULL,
                    created_at REAL NOT NULL,
                    PRIMARY KEY (content_hash, loader_version)
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS quarantine (
                    id INTEGER PRIMARY KEY,
                    rel_path TEXT NOT NULL UNIQUE,
                    content_hash TEXT NOT NULL,
                    error_type TEXT NOT NULL,
                    error_message TEXT NOT NULL,
                    updated_at REAL NOT NULL
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS candidate_buckets (
                    bucket_key TEXT NOT NULL,
                    family TEXT NOT NULL,
                    file_id INTEGER NOT NULL,
                    PRIMARY KEY (bucket_key, file_id),
                    FOREIGN KEY (file_id) REFERENCES files(id) ON DELETE CASCADE
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS candidates (
                    pair_a INTEGER NOT NULL,
                    pair_b INTEGER NOT NULL,
                    family TEXT NOT NULL,
                    bucket_hits INTEGER NOT NULL DEFAULT 0,
                    PRIMARY KEY (pair_a, pair_b),
                    FOREIGN KEY (pair_a) REFERENCES files(id) ON DELETE CASCADE,
                    FOREIGN KEY (pair_b) REFERENCES files(id) ON DELETE CASCADE
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS verification_cache (
                    content_a TEXT NOT NULL,
                    content_b TEXT NOT NULL,
                    family TEXT NOT NULL,
                    fingerprint_version TEXT NOT NULL,
                    same INTEGER NOT NULL,
                    score REAL NOT NULL,
                    detail TEXT NOT NULL,
                    created_at REAL NOT NULL,
                    PRIMARY KEY (content_a, content_b, family, fingerprint_version)
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS verified_pairs (
                    pair_a INTEGER NOT NULL,
                    pair_b INTEGER NOT NULL,
                    family TEXT NOT NULL,
                    score REAL NOT NULL,
                    detail TEXT NOT NULL,
                    PRIMARY KEY (pair_a, pair_b),
                    FOREIGN KEY (pair_a) REFERENCES files(id) ON DELETE CASCADE,
                    FOREIGN KEY (pair_b) REFERENCES files(id) ON DELETE CASCADE
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS clusters (
                    cluster_id TEXT PRIMARY KEY,
                    family TEXT NOT NULL,
                    member_count INTEGER NOT NULL,
                    created_at REAL NOT NULL
                )
                """
            )
            cursor.execute(
                """
                CREATE TABLE IF NOT EXISTS cluster_members (
                    file_id INTEGER PRIMARY KEY,
                    cluster_id TEXT NOT NULL,
                    FOREIGN KEY (file_id) REFERENCES files(id) ON DELETE CASCADE,
                    FOREIGN KEY (cluster_id) REFERENCES clusters(cluster_id) ON DELETE CASCADE
                )
                """
            )
            cursor.execute(
                "CREATE INDEX IF NOT EXISTS idx_files_hash ON files(content_hash)"
            )
            cursor.execute(
                "CREATE INDEX IF NOT EXISTS idx_files_status_family ON files(status, family)"
            )
            cursor.execute(
                "CREATE INDEX IF NOT EXISTS idx_buckets_lookup ON candidate_buckets(family, bucket_key, file_id)"
            )
            cursor.execute(
                "CREATE INDEX IF NOT EXISTS idx_cluster_family ON clusters(family)"
            )
            cursor.execute(
                "INSERT OR REPLACE INTO schema_meta(key, value) VALUES ('schema_version', ?)",
                (str(SCHEMA_VERSION),),
            )

    def get_file_by_path(self, rel_path: str) -> sqlite3.Row | None:
        return self.connection.execute(
            "SELECT * FROM files WHERE rel_path = ?", (rel_path,)
        ).fetchone()

    def all_files(self) -> list[sqlite3.Row]:
        return list(self.connection.execute("SELECT * FROM files ORDER BY rel_path"))

    def successful_files(self) -> list[sqlite3.Row]:
        return list(
            self.connection.execute(
                "SELECT * FROM files WHERE status = 'ok' ORDER BY rel_path"
            )
        )

    def file_by_id(self, file_id: int) -> sqlite3.Row | None:
        return self.connection.execute("SELECT * FROM files WHERE id = ?", (file_id,)).fetchone()

    def lookup_decode_cache(self, content_hash: str, loader_version: str) -> sqlite3.Row | None:
        return self.connection.execute(
            """
            SELECT * FROM decode_cache
            WHERE content_hash = ? AND loader_version = ?
            """,
            (content_hash, loader_version),
        ).fetchone()

    def put_decode_cache(
        self,
        content_hash: str,
        family: str,
        loader_version: str,
        payload: bytes,
        hashes: Sequence[str],
        metadata: dict[str, Any],
    ) -> None:
        with self.transaction() as cursor:
            cursor.execute(
                """
                INSERT INTO decode_cache(
                    content_hash, family, loader_version, payload,
                    candidate_hashes, metadata, created_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(content_hash, loader_version) DO UPDATE SET
                    family = excluded.family,
                    payload = excluded.payload,
                    candidate_hashes = excluded.candidate_hashes,
                    metadata = excluded.metadata,
                    created_at = excluded.created_at
                """,
                (
                    content_hash,
                    family,
                    loader_version,
                    payload,
                    json.dumps(list(hashes), sort_keys=True),
                    json.dumps(metadata, ensure_ascii=False, sort_keys=True),
                    time.time(),
                ),
            )

    def upsert_success(
        self,
        *,
        rel_path: str,
        kind: MediaKind,
        content_hash: str,
        size: int,
        mtime_ns: int,
        payload: bytes,
        hashes: Sequence[str],
        metadata: dict[str, Any],
        loader_name: str,
        loader_version: str,
    ) -> int:
        now = time.time()
        with self.transaction() as cursor:
            cursor.execute(
                """
                INSERT INTO files(
                    rel_path, family, format, content_hash, size, mtime_ns, status,
                    fingerprint, candidate_hashes, metadata, loader_name,
                    loader_version, error_type, error_message, first_seen, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, 'ok', ?, ?, ?, ?, ?, NULL, NULL, ?, ?)
                ON CONFLICT(rel_path) DO UPDATE SET
                    family = excluded.family,
                    format = excluded.format,
                    content_hash = excluded.content_hash,
                    size = excluded.size,
                    mtime_ns = excluded.mtime_ns,
                    status = 'ok',
                    fingerprint = excluded.fingerprint,
                    candidate_hashes = excluded.candidate_hashes,
                    metadata = excluded.metadata,
                    loader_name = excluded.loader_name,
                    loader_version = excluded.loader_version,
                    error_type = NULL,
                    error_message = NULL,
                    updated_at = excluded.updated_at
                """,
                (
                    rel_path,
                    kind.family.value,
                    kind.format,
                    content_hash,
                    int(size),
                    int(mtime_ns),
                    payload,
                    json.dumps(list(hashes), sort_keys=True),
                    json.dumps(metadata, ensure_ascii=False, sort_keys=True),
                    loader_name,
                    loader_version,
                    now,
                    now,
                ),
            )
            cursor.execute("DELETE FROM quarantine WHERE rel_path = ?", (rel_path,))
            row = cursor.execute("SELECT id FROM files WHERE rel_path = ?", (rel_path,)).fetchone()
        return int(row["id"])

    def touch_success(self, rel_path: str, size: int, mtime_ns: int) -> None:
        with self.transaction() as cursor:
            cursor.execute(
                "UPDATE files SET size = ?, mtime_ns = ?, updated_at = ? WHERE rel_path = ?",
                (int(size), int(mtime_ns), time.time(), rel_path),
            )

    def mark_failed(
        self,
        *,
        rel_path: str,
        kind: MediaKind,
        content_hash: str,
        size: int,
        mtime_ns: int,
        error_type: str,
        error_message: str,
    ) -> int:
        now = time.time()
        message = (error_message or "")[:1000]
        with self.transaction() as cursor:
            cursor.execute(
                """
                INSERT INTO files(
                    rel_path, family, format, content_hash, size, mtime_ns, status,
                    fingerprint, candidate_hashes, metadata, loader_name,
                    loader_version, error_type, error_message, first_seen, updated_at
                ) VALUES (?, ?, ?, ?, ?, ?, 'failed', NULL, '[]', '{}', NULL,
                          NULL, ?, ?, ?, ?)
                ON CONFLICT(rel_path) DO UPDATE SET
                    family = excluded.family,
                    format = excluded.format,
                    content_hash = excluded.content_hash,
                    size = excluded.size,
                    mtime_ns = excluded.mtime_ns,
                    status = 'failed',
                    fingerprint = NULL,
                    candidate_hashes = '[]',
                    metadata = '{}',
                    loader_name = NULL,
                    loader_version = NULL,
                    error_type = excluded.error_type,
                    error_message = excluded.error_message,
                    updated_at = excluded.updated_at
                """,
                (
                    rel_path,
                    kind.family.value,
                    kind.format,
                    content_hash,
                    int(size),
                    int(mtime_ns),
                    error_type,
                    message,
                    now,
                    now,
                ),
            )
            cursor.execute(
                """
                INSERT INTO quarantine(rel_path, content_hash, error_type, error_message, updated_at)
                VALUES (?, ?, ?, ?, ?)
                ON CONFLICT(rel_path) DO UPDATE SET
                    content_hash = excluded.content_hash,
                    error_type = excluded.error_type,
                    error_message = excluded.error_message,
                    updated_at = excluded.updated_at
                """,
                (rel_path, content_hash, error_type, message, now),
            )
            row = cursor.execute("SELECT id FROM files WHERE rel_path = ?", (rel_path,)).fetchone()
        return int(row["id"])

    def delete_missing(self, seen_rel_paths: Iterable[str]) -> int:
        seen = set(seen_rel_paths)
        deleted = 0
        with self.transaction() as cursor:
            rows = list(cursor.execute("SELECT id, rel_path FROM files"))
            for row in rows:
                if row["rel_path"] not in seen:
                    cursor.execute("DELETE FROM files WHERE id = ?", (row["id"],))
                    cursor.execute("DELETE FROM quarantine WHERE rel_path = ?", (row["rel_path"],))
                    deleted += 1
        return deleted

    def remove_path(self, rel_path: str) -> bool:
        with self.transaction() as cursor:
            row = cursor.execute("SELECT id FROM files WHERE rel_path = ?", (rel_path,)).fetchone()
            if row is None:
                return False
            cursor.execute("DELETE FROM files WHERE id = ?", (row["id"],))
            cursor.execute("DELETE FROM quarantine WHERE rel_path = ?", (rel_path,))
        return True

    def clear_candidate_tables(self) -> None:
        with self.transaction() as cursor:
            cursor.execute("DELETE FROM candidates")
            cursor.execute("DELETE FROM candidate_buckets")

    def rebuild_candidate_tables(self) -> dict[str, int]:
        """Split each 64-bit hash into four bands and compare only bucket peers."""

        files = self.successful_files()
        bucket_members: dict[tuple[str, int, int], list[int]] = {}
        bucket_rows: list[tuple[str, str, int]] = []
        for row in files:
            hashes = json.loads(row["candidate_hashes"] or "[]")
            for value in hashes:
                try:
                    bands = split_hash_bands(str(value))
                except (TypeError, ValueError):
                    continue
                for band_index, band_value in enumerate(bands):
                    key = (str(row["family"]), band_index, int(band_value))
                    bucket_members.setdefault(key, []).append(int(row["id"]))
                    bucket_key = f"{row['family']}:{band_index}:{int(band_value):04x}"
                    bucket_rows.append((bucket_key, str(row["family"]), int(row["id"])))

        pairs: set[tuple[int, int, str]] = set()
        bucket_pair_hits: dict[tuple[int, int, str], int] = {}
        for (family, _band_index, _band_value), members in bucket_members.items():
            unique_members = sorted(set(members))
            for left, right in itertools.combinations(unique_members, 2):
                pair = (left, right, family)
                pairs.add(pair)
                bucket_pair_hits[pair] = bucket_pair_hits.get(pair, 0) + 1

        with self.transaction() as cursor:
            cursor.execute("DELETE FROM candidates")
            cursor.execute("DELETE FROM candidate_buckets")
            cursor.executemany(
                "INSERT OR IGNORE INTO candidate_buckets(bucket_key, family, file_id) VALUES (?, ?, ?)",
                bucket_rows,
            )
            cursor.executemany(
                "INSERT INTO candidates(pair_a, pair_b, family, bucket_hits) VALUES (?, ?, ?, ?)",
                [(a, b, family, bucket_pair_hits[(a, b, family)]) for a, b, family in sorted(pairs)],
            )
        return {
            "bucket_count": len(bucket_members),
            "candidate_pairs": len(pairs),
            "bucket_entries": len(bucket_rows),
        }

    def candidate_pairs(self) -> list[sqlite3.Row]:
        return list(
            self.connection.execute(
                "SELECT * FROM candidates ORDER BY family, pair_a, pair_b"
            )
        )

    def get_verification_cache(
        self, content_a: str, content_b: str, family: str, fingerprint_version: str
    ) -> sqlite3.Row | None:
        left, right = sorted((content_a, content_b))
        return self.connection.execute(
            """
            SELECT * FROM verification_cache
            WHERE content_a = ? AND content_b = ? AND family = ? AND fingerprint_version = ?
            """,
            (left, right, family, fingerprint_version),
        ).fetchone()

    def put_verification_cache(
        self,
        content_a: str,
        content_b: str,
        family: str,
        fingerprint_version: str,
        same: bool,
        score: float,
        detail: dict[str, Any],
    ) -> None:
        left, right = sorted((content_a, content_b))
        with self.transaction() as cursor:
            cursor.execute(
                """
                INSERT INTO verification_cache(
                    content_a, content_b, family, fingerprint_version,
                    same, score, detail, created_at
                ) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
                ON CONFLICT(content_a, content_b, family, fingerprint_version) DO UPDATE SET
                    same = excluded.same,
                    score = excluded.score,
                    detail = excluded.detail,
                    created_at = excluded.created_at
                """,
                (
                    left,
                    right,
                    family,
                    fingerprint_version,
                    int(bool(same)),
                    float(score),
                    json.dumps(detail, ensure_ascii=False, sort_keys=True, default=float),
                    time.time(),
                ),
            )

    def replace_verified_pairs(self, rows: Sequence[tuple[int, int, str, float, dict[str, Any]]]) -> None:
        with self.transaction() as cursor:
            cursor.execute("DELETE FROM verified_pairs")
            cursor.executemany(
                """
                INSERT INTO verified_pairs(pair_a, pair_b, family, score, detail)
                VALUES (?, ?, ?, ?, ?)
                """,
                [
                    (
                        int(a),
                        int(b),
                        family,
                        float(score),
                        json.dumps(detail, ensure_ascii=False, sort_keys=True, default=float),
                    )
                    for a, b, family, score, detail in rows
                ],
            )

    def verified_pairs(self) -> list[sqlite3.Row]:
        return list(self.connection.execute("SELECT * FROM verified_pairs ORDER BY pair_a, pair_b"))

    def write_clusters(self, clusters: Sequence[dict[str, Any]]) -> None:
        """Atomically replace all clusters; an interruption cannot expose half a cluster."""

        with self.transaction() as cursor:
            cursor.execute("DELETE FROM cluster_members")
            cursor.execute("DELETE FROM clusters")
            now = time.time()
            for cluster in clusters:
                cursor.execute(
                    "INSERT INTO clusters(cluster_id, family, member_count, created_at) VALUES (?, ?, ?, ?)",
                    (
                        cluster["cluster_id"],
                        cluster["family"],
                        len(cluster["member_ids"]),
                        now,
                    ),
                )
                cursor.executemany(
                    "INSERT INTO cluster_members(file_id, cluster_id) VALUES (?, ?)",
                    [(int(file_id), cluster["cluster_id"]) for file_id in cluster["member_ids"]],
                )

    def clear_clusters(self) -> None:
        """Mark the cluster snapshot stale before a potentially interrupted run."""

        with self.transaction() as cursor:
            cursor.execute("DELETE FROM cluster_members")
            cursor.execute("DELETE FROM clusters")

    def prune_orphan_clusters(self) -> int:
        with self.transaction() as cursor:
            cursor.execute(
                """
                DELETE FROM clusters
                WHERE cluster_id NOT IN (SELECT DISTINCT cluster_id FROM cluster_members)
                """
            )
            return int(cursor.rowcount)

    def clusters(self) -> list[sqlite3.Row]:
        return list(self.connection.execute("SELECT * FROM clusters ORDER BY cluster_id"))

    def cluster_members(self) -> list[sqlite3.Row]:
        return list(self.connection.execute("SELECT * FROM cluster_members ORDER BY cluster_id, file_id"))

    def cluster_records(self) -> list[dict[str, Any]]:
        records: dict[str, dict[str, Any]] = {}
        for cluster in self.clusters():
            records[cluster["cluster_id"]] = {
                "cluster_id": cluster["cluster_id"],
                "family": cluster["family"],
                "member_count": int(cluster["member_count"]),
                "members": [],
            }
        members = self.connection.execute(
            """
            SELECT cm.cluster_id, f.id, f.rel_path, f.content_hash
            FROM cluster_members cm JOIN files f ON f.id = cm.file_id
            ORDER BY cm.cluster_id, f.rel_path
            """
        )
        for row in members:
            if row["cluster_id"] in records:
                records[row["cluster_id"]]["members"].append(
                    {
                        "file_id": int(row["id"]),
                        "path": row["rel_path"],
                        "content_hash": row["content_hash"],
                    }
                )
        return list(records.values())

    def load_signature_payload(self, row: sqlite3.Row) -> dict[str, Any]:
        if row["fingerprint"] is None:
            raise StoreError(f"file {row['rel_path']} has no fingerprint")
        return unpack_fingerprint(row["fingerprint"])

    def lookup_cached_fingerprint(
        self, content_hash: str, loader_version: str
    ) -> tuple[bytes, list[str], dict[str, Any], str] | None:
        row = self.lookup_decode_cache(content_hash, loader_version)
        if row is None:
            return None
        hashes = json.loads(row["candidate_hashes"] or "[]")
        metadata = json.loads(row["metadata"] or "{}")
        return row["payload"], hashes, metadata, row["family"]

    def counts(self) -> dict[str, int]:
        rows = self.connection.execute(
            "SELECT status, COUNT(*) AS count FROM files GROUP BY status"
        )
        values = {row["status"]: int(row["count"]) for row in rows}
        return {
            "total": int(values.get("ok", 0) + values.get("failed", 0)),
            "success": int(values.get("ok", 0)),
            "failed": int(values.get("failed", 0)),
        }

    def group_counts(self) -> dict[str, dict[str, int]]:
        result: dict[str, dict[str, int]] = {}
        rows = self.connection.execute(
            """
            SELECT family, status, COUNT(*) AS count
            FROM files GROUP BY family, status
            """
        )
        for row in rows:
            family = row["family"]
            result.setdefault(family, {"total": 0, "success": 0, "failed": 0})
            status_key = "success" if row["status"] == "ok" else str(row["status"])
            result[family][status_key] = int(row["count"])
            result[family]["total"] += int(row["count"])
        return result

    def cluster_counts_by_family(self) -> dict[str, dict[str, int]]:
        result: dict[str, dict[str, int]] = {}
        rows = self.connection.execute(
            "SELECT family, COUNT(*) AS count, SUM(member_count) AS members FROM clusters GROUP BY family"
        )
        for row in rows:
            result[row["family"]] = {
                "clusters": int(row["count"]),
                "members": int(row["members"]),
            }
        return result

    def duplicate_cluster_counts_by_family(self) -> dict[str, int]:
        rows = self.connection.execute(
            "SELECT family, COUNT(*) AS count FROM clusters WHERE member_count > 1 GROUP BY family"
        )
        return {row["family"]: int(row["count"]) for row in rows}

    def candidate_counts_by_family(self) -> dict[str, int]:
        rows = self.connection.execute(
            "SELECT family, COUNT(*) AS count FROM candidates GROUP BY family"
        )
        return {row["family"]: int(row["count"]) for row in rows}

    def verified_counts_by_family(self) -> dict[str, int]:
        rows = self.connection.execute(
            "SELECT family, COUNT(*) AS count FROM verified_pairs GROUP BY family"
        )
        return {row["family"]: int(row["count"]) for row in rows}

    def quarantined(self) -> list[sqlite3.Row]:
        return list(self.connection.execute("SELECT * FROM quarantine ORDER BY rel_path"))

    def candidate_hash_values(self, row: sqlite3.Row) -> list[str]:
        return list(json.loads(row["candidate_hashes"] or "[]"))
