"""Connector factory with per-source instance reuse."""
from __future__ import annotations

import os

from .csv_source import CSVSource
from .jsonl_source import JSONLSource
from .sqlite_source import SQLiteSource


class ConnectorRegistry:
    def __init__(self):
        self._instances = {}

    def get(self, source):
        key = (source.type, os.path.abspath(source.path), source.table)
        if key not in self._instances:
            self._instances[key] = self._create(source)
        return self._instances[key]

    def replay_file(self, path: str, name: str = "quarantine"):
        """Open a quarantine CSV back through the connector layer."""
        key = ("csv", os.path.abspath(path), "__replay__")
        if key not in self._instances:
            self._instances[key] = CSVSource(name, path)
        return self._instances[key]

    @staticmethod
    def _create(source):
        if source.type == "csv":
            return CSVSource(source.name, source.path)
        if source.type == "jsonl":
            return JSONLSource(source.name, source.path)
        if source.type == "sqlite":
            return SQLiteSource(source.name, source.path, source.table)
        raise ValueError(f"unknown source type: {source.type!r}")

    def close_all(self) -> None:
        for conn in self._instances.values():
            conn.close()
        self._instances.clear()
