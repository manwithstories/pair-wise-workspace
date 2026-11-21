"""Append-only packet journal used as the resumable temporary artifact.

Repaired packets are appended to the journal at packet boundaries; the
checkpoint records how many bytes were committed.  An interrupted run
truncates the journal back to the committed boundary and keeps appending,
and the final MP4 is muxed from the journal in a single deterministic pass,
so a resumed output is byte-identical to an uninterrupted one.
"""

from __future__ import annotations

import base64
import json
from dataclasses import dataclass
from fractions import Fraction
from pathlib import Path
from typing import Iterator


@dataclass
class JournalRecord:
    stream_index: int
    kind: str
    pts: Fraction | None
    dts: Fraction | None
    duration: Fraction
    is_keyframe: bool
    payload: bytes


def _frac_to_list(value: Fraction | None) -> list[int] | None:
    if value is None:
        return None
    return [value.numerator, value.denominator]


def _frac_from_list(value: list[int] | None) -> Fraction | None:
    if value is None:
        return None
    return Fraction(value[0], value[1])


class JournalWriter:
    """Appends repaired packets; ``tell()`` gives the committed boundary."""

    def __init__(self, path: str | Path, header: dict | None = None):
        self.path = Path(path)
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self._fh = self.path.open("ab")
        if header is not None:
            self._write_line({"header": header})

    def _write_line(self, obj: dict) -> None:
        self._fh.write(json.dumps(obj, separators=(",", ":")).encode("utf-8"))
        self._fh.write(b"\n")

    def append(self, record: JournalRecord) -> int:
        """Append one record and return the new committed byte boundary."""
        self._write_line(
            {
                "stream_index": record.stream_index,
                "kind": record.kind,
                "pts": _frac_to_list(record.pts),
                "dts": _frac_to_list(record.dts),
                "duration": _frac_to_list(record.duration),
                "keyframe": record.is_keyframe,
                "payload": base64.b64encode(record.payload).decode("ascii"),
            }
        )
        self._fh.flush()
        return self._fh.tell()

    def tell(self) -> int:
        return self._fh.tell()

    def close(self) -> None:
        self._fh.close()

    def __enter__(self) -> "JournalWriter":
        return self

    def __exit__(self, *exc: object) -> None:
        self.close()


def read_journal(path: str | Path) -> Iterator[JournalRecord | dict]:
    """Yield the header dict first, then each journal record in order."""
    with Path(path).open("rb") as fh:
        for line in fh:
            obj = json.loads(line)
            if "header" in obj:
                yield obj["header"]
                continue
            yield JournalRecord(
                stream_index=obj["stream_index"],
                kind=obj["kind"],
                pts=_frac_from_list(obj["pts"]),
                dts=_frac_from_list(obj["dts"]),
                duration=_frac_from_list(obj["duration"]) or Fraction(0),
                is_keyframe=bool(obj["keyframe"]),
                payload=base64.b64decode(obj["payload"]),
            )
