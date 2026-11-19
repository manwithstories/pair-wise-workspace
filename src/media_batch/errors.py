"""Error model: every recoverable failure carries source file, byte offset and record index."""

from __future__ import annotations

import json
from dataclasses import dataclass, field, asdict
from pathlib import Path


@dataclass
class MediaError:
    code: str               # machine-readable, e.g. "invalid_time", "truncated_record"
    message: str
    source: str             # source file path (or "" for pipeline-level errors)
    source_offset: int      # byte offset within source; -1 when not applicable
    record_index: int       # ordinal of the record within the source; -1 when n/a
    severity: str = "error"  # "error" | "warning"

    def to_dict(self) -> dict:
        return asdict(self)


class MediaBatchError(Exception):
    """Fatal (non-recoverable) error; CLI maps this to a non-zero exit status."""

    def __init__(self, code: str, message: str, source: str = "", source_offset: int = -1):
        super().__init__(message)
        self.error = MediaError(code, message, source, source_offset, -1)


@dataclass
class ErrorLog:
    """Ordered collection of recoverable errors; persisted as JSON for resume."""

    errors: list[MediaError] = field(default_factory=list)

    def add(self, code: str, message: str, source: str = "",
            source_offset: int = -1, record_index: int = -1,
            severity: str = "error") -> MediaError:
        err = MediaError(code, message, source, source_offset, record_index, severity)
        self.errors.append(err)
        return err

    def __len__(self) -> int:
        return len(self.errors)

    def __iter__(self):
        return iter(self.errors)

    def by_code(self, code: str) -> list[MediaError]:
        return [e for e in self.errors if e.code == code]

    def to_json(self) -> str:
        return json.dumps([e.to_dict() for e in self.errors], ensure_ascii=False, indent=2)

    def save(self, path: str | Path) -> None:
        Path(path).write_text(self.to_json() + "\n", encoding="utf-8")

    @classmethod
    def load(cls, path: str | Path) -> "ErrorLog":
        log = cls()
        p = Path(path)
        if p.exists():
            for d in json.loads(p.read_text(encoding="utf-8")):
                log.errors.append(MediaError(**d))
        return log
