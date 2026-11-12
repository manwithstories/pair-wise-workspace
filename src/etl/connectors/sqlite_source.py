"""SQLite connector with chunked cursor reads."""
from __future__ import annotations

import sqlite3

import pandas as pd

from .base import infer_series_type

_TYPE_MAP = {
    "INT": "int", "INTEGER": "int", "BIGINT": "int", "SMALLINT": "int",
    "REAL": "decimal", "FLOAT": "decimal", "DOUBLE": "decimal", "NUMERIC": "decimal",
    "TEXT": "string", "CHAR": "string", "VARCHAR": "string", "CLOB": "string",
}
_SCHEMA_ROWS = 200


class SQLiteSource:
    def __init__(self, name: str, path: str, table: str):
        self.name = name
        self.path = path
        self.table = table
        self._conn = None

    def _connect(self) -> sqlite3.Connection:
        if self._conn is None:
            self._conn = sqlite3.connect(self.path)
        return self._conn

    def schema(self) -> dict:
        conn = self._connect()
        info = conn.execute(f'PRAGMA table_info("{self.table}")').fetchall()
        if not info:
            raise ValueError(f"{self.path}: table {self.table!r} not found")
        schema = {}
        for _, column, decl, *_ in info:
            base = (decl or "TEXT").split("(")[0].strip().upper()
            inferred = _TYPE_MAP.get(base, "string")
            if inferred == "string":
                rows = conn.execute(
                    f'SELECT "{column}" FROM "{self.table}" '
                    f'WHERE "{column}" IS NOT NULL LIMIT {_SCHEMA_ROWS}'
                ).fetchall()
                inferred = infer_series_type(pd.Series([r[0] for r in rows]))
            schema[column] = inferred
        return schema

    def iter_batches(self, batch_size: int):
        conn = self._connect()
        cursor = conn.execute(f'SELECT * FROM "{self.table}"')
        columns = [d[0] for d in cursor.description]
        while True:
            rows = cursor.fetchmany(batch_size)
            if not rows:
                break
            yield pd.DataFrame(rows, columns=columns)

    def close(self) -> None:
        if self._conn is not None:
            self._conn.close()
            self._conn = None
