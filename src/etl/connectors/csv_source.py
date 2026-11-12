"""CSV connector with encoding and delimiter sniffing."""
from __future__ import annotations

import csv

import pandas as pd

from .base import infer_series_type

_ENCODINGS = ("utf-8-sig", "utf-8", "gb18030", "latin1")
_SAMPLE_BYTES = 65536
_SCHEMA_ROWS = 200


class CSVSource:
    def __init__(self, name: str, path: str):
        self.name = name
        self.path = path
        self._encoding = None
        self._delimiter = None

    def _sniff(self) -> None:
        if self._encoding is not None:
            return
        with open(self.path, "rb") as fh:
            raw = fh.read(_SAMPLE_BYTES)
        text = None
        for enc in _ENCODINGS:
            try:
                text = raw.decode(enc)
                self._encoding = enc
                break
            except UnicodeDecodeError:
                continue
        if text is None:
            text = raw.decode("latin1", errors="replace")
            self._encoding = "latin1"
        try:
            dialect = csv.Sniffer().sniff(text[:8192], delimiters=",;\t|")
            self._delimiter = dialect.delimiter
        except csv.Error:
            first_line = text.splitlines()[0] if text.splitlines() else ""
            counts = {d: first_line.count(d) for d in ",;\t|"}
            best = max(counts, key=counts.get)
            self._delimiter = best if counts[best] > 0 else ","

    def _read(self, **kwargs):
        self._sniff()
        return pd.read_csv(
            self.path,
            encoding=self._encoding,
            sep=self._delimiter,
            dtype=str,
            keep_default_na=False,
            **kwargs,
        )

    def schema(self) -> dict:
        sample = self._read(nrows=_SCHEMA_ROWS)
        return {col: infer_series_type(sample[col]) for col in sample.columns}

    def iter_batches(self, batch_size: int):
        yield from self._read(chunksize=batch_size)

    def close(self) -> None:
        pass
