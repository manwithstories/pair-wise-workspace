"""Idempotent SQLite sink built on a staging table.

Merge rules:
  * same pk across sources  -> lowest priority number wins
  * same pk within a source -> newest updated_at wins
  * fully identical rows    -> counted as dup_resolved
  * same pk+source+updated_at with different payloads -> unmerged (exit 3)
Re-running with the same input rebuilds the target table deterministically,
so row count and checksum are stable. The staging table is always dropped.
"""
from __future__ import annotations

import hashlib
import sqlite3

_SQL_TYPES = {
    "int": "INTEGER",
    "decimal": "REAL",
    "float": "REAL",
    "datetime": "TEXT",
    "string": "TEXT",
}


class SQLiteSink:
    def __init__(self, db_path: str, table: str, target_schema: dict, pk_field: str):
        self.db_path = db_path
        self.table = table
        self.staging = f"_staging_{table}"
        self.columns = list(target_schema)
        self.pk = pk_field
        self._types = {f: _SQL_TYPES[spec["type"]] for f, spec in target_schema.items()}
        self._conn = sqlite3.connect(db_path)
        # Bound SQLite's page cache so window-function merges over large
        # staging tables spill to temp files instead of growing RSS.
        self._conn.execute("PRAGMA cache_size = -8000")   # 8 MB
        self._conn.execute("PRAGMA temp_store = FILE")

    def begin(self) -> None:
        cur = self._conn.cursor()
        cur.execute(f'DROP TABLE IF EXISTS "{self.staging}"')
        cols = ", ".join(f'"{c}" {self._types[c]}' for c in self.columns)
        cur.execute(
            f'CREATE TABLE IF NOT EXISTS "{self.table}" '
            f'({cols}, PRIMARY KEY ("{self.pk}"))'
        )
        cur.execute(
            f'CREATE TABLE "{self.staging}" '
            f'({cols}, "_source" TEXT, "_priority" INTEGER, "_hash" TEXT)'
        )
        self._conn.commit()

    def write_batch(self, frame, source: str, priority: int) -> None:
        if frame.empty:
            return
        frame["_source"] = source
        frame["_priority"] = priority
        frame["_hash"] = [
            hashlib.md5("|".join(map(str, row)).encode()).hexdigest()
            for row in frame[self.columns].itertuples(index=False, name=None)
        ]
        frame.to_sql(self.staging, self._conn, if_exists="append",
                     index=False, chunksize=10000)

    def merge(self) -> dict:
        cur = self._conn.cursor()
        order_col = "updated_at" if "updated_at" in self.columns else self.pk
        rows_in = cur.execute(f'SELECT COUNT(*) FROM "{self.staging}"').fetchone()[0]
        distinct_pk = cur.execute(
            f'SELECT COUNT(DISTINCT "{self.pk}") FROM "{self.staging}"'
        ).fetchone()[0]
        unmerged = [
            r[0]
            for r in cur.execute(
                f'SELECT "{self.pk}" FROM "{self.staging}" '
                f'GROUP BY "{self.pk}", "_source", "{order_col}" '
                f'HAVING COUNT(DISTINCT "_hash") > 1'
            ).fetchall()
        ]
        cols = ", ".join(f'"{c}"' for c in self.columns)
        cur.execute(f'DELETE FROM "{self.table}"')
        if unmerged:
            placeholders = ",".join("?" * len(unmerged))
            cur.execute(
                f'INSERT INTO "{self.table}" ({cols}) SELECT {cols} '
                f'FROM (SELECT *, ROW_NUMBER() OVER ('
                f'  PARTITION BY "{self.pk}" '
                f'  ORDER BY "_priority" ASC, "{order_col}" DESC, rowid ASC'
                f') AS "_rn" FROM "{self.staging}") WHERE "_rn" = 1 '
                f'AND "{self.pk}" NOT IN ({placeholders})',
                unmerged,
            )
        else:
            cur.execute(
                f'INSERT INTO "{self.table}" ({cols}) SELECT {cols} '
                f'FROM (SELECT *, ROW_NUMBER() OVER ('
                f'  PARTITION BY "{self.pk}" '
                f'  ORDER BY "_priority" ASC, "{order_col}" DESC, rowid ASC'
                f') AS "_rn" FROM "{self.staging}") WHERE "_rn" = 1'
            )
        loaded = cur.execute(f'SELECT COUNT(*) FROM "{self.table}"').fetchone()[0]
        self._conn.commit()
        return {
            "staging_rows": rows_in,
            "distinct_pk": distinct_pk,
            "dup_resolved": rows_in - distinct_pk,
            "unmerged": len(unmerged),
            "unmerged_keys": [str(k) for k in unmerged],
            "loaded": loaded,
        }

    def checksum(self) -> str:
        digest = hashlib.md5()
        cursor = self._conn.execute(f'SELECT * FROM "{self.table}" ORDER BY "{self.pk}"')
        for row in cursor:
            digest.update(repr(tuple(row)).encode())
        return digest.hexdigest()[:4]

    def cleanup(self) -> None:
        self._conn.execute(f'DROP TABLE IF EXISTS "{self.staging}"')
        self._conn.commit()

    def close(self) -> None:
        self._conn.close()
