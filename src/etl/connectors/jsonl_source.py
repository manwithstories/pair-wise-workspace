"""JSONL connector with dotted-path flattening of nested objects."""
from __future__ import annotations

import json

import pandas as pd

from .base import infer_series_type

_SCHEMA_LINES = 200


class FlattenConflictError(Exception):
    """Raised when two nested paths flatten to the same column name."""


def flatten_record(record: dict, prefix: str = "") -> dict:
    out = {}
    for key, value in record.items():
        path = f"{prefix}.{key}" if prefix else key
        if isinstance(value, dict):
            for nkey, nval in flatten_record(value, path).items():
                if nkey in out:
                    raise FlattenConflictError(f"flattened key collision: {nkey!r}")
                out[nkey] = nval
        else:
            # Arrays are kept as-is; the transform layer rejects them with
            # reason=array_rejected instead of silently expanding them.
            if path in out:
                raise FlattenConflictError(f"flattened key collision: {path!r}")
            out[path] = value
    return out


class JSONLSource:
    def __init__(self, name: str, path: str):
        self.name = name
        self.path = path

    def _iter_records(self):
        with open(self.path, encoding="utf-8") as fh:
            for lineno, line in enumerate(fh, 1):
                line = line.strip()
                if not line:
                    continue
                try:
                    record = json.loads(line)
                except json.JSONDecodeError as exc:
                    raise ValueError(f"{self.path}:{lineno}: invalid JSON: {exc}")
                if not isinstance(record, dict):
                    raise ValueError(f"{self.path}:{lineno}: top-level object required")
                try:
                    yield flatten_record(record)
                except FlattenConflictError as exc:
                    raise FlattenConflictError(f"{self.path}:{lineno}: {exc}")

    def schema(self) -> dict:
        records = []
        for idx, rec in enumerate(self._iter_records()):
            if idx >= _SCHEMA_LINES:
                break
            records.append(rec)
        frame = pd.DataFrame(records)
        return {col: infer_series_type(frame[col]) for col in frame.columns}

    def iter_batches(self, batch_size: int):
        buf = []
        for rec in self._iter_records():
            buf.append(rec)
            if len(buf) >= batch_size:
                yield pd.DataFrame(buf)
                buf = []
        if buf:
            yield pd.DataFrame(buf)

    def close(self) -> None:
        pass
