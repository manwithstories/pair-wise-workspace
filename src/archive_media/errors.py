"""Structured error model shared by every recovery module.

Every problem found while parsing a manifest, demuxing a segment or verifying
the archive is reported through :class:`RecoveryError` / :class:`Diagnostic`
with the same locator fields:

* ``manifest_path`` -- the playlist / MPD the reference came from,
* ``representation`` -- the variant / adaptation set / representation id,
* ``sequence`` -- the media sequence number of the segment,
* ``time_range`` -- ``(start_seconds, end_seconds)`` on the programme
  timeline, and
* ``segment_ref`` -- the raw, unmodified URI reference from the manifest.

Exit-code policy (used by the CLI):

* 0 -- success, including recovered-but-damaged segments,
* 2 -- a fatal damage under the selected recovery policy (e.g. ``--strict``),
* 3 -- invalid parameters or manifest syntax.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Iterable


class Code(str, Enum):
    """Stable error codes; their string values are part of the JSON output."""

    # --- manifest layer ----------------------------------------------
    MANIFEST_SYNTAX = "ERR_MANIFEST_SYNTAX"
    MANIFEST_UNSUPPORTED = "ERR_MANIFEST_UNSUPPORTED"
    REFERENCE_MISSING = "ERR_REFERENCE_MISSING"
    REFERENCE_ESCAPE = "ERR_REFERENCE_ESCAPE"
    DUPLICATE_REFERENCE = "ERR_DUPLICATE_REFERENCE"
    REORDERED_REFERENCE = "ERR_REORDERED_REFERENCE"
    TIMESTAMP_COLLISION = "ERR_TIMESTAMP_COLLISION"
    MISSING_INDEX_ENTRY = "ERR_MISSING_INDEX_ENTRY"

    # --- demux layer -------------------------------------------------
    SEGMENT_TRUNCATED = "ERR_SEGMENT_TRUNCATED"
    PACKET_CORRUPT = "ERR_PACKET_CORRUPT"
    DECODE_FAILURE = "ERR_DECODE_FAILURE"
    TIMESTAMP_DISCONTINUITY = "ERR_TIMESTAMP_DISCONTINUITY"

    # --- audio layer -------------------------------------------------
    UNSUPPORTED_LAYOUT = "ERR_UNSUPPORTED_LAYOUT"
    LAYOUT_MISMATCH = "ERR_LAYOUT_MISMATCH"
    RESAMPLE_FAILURE = "ERR_RESAMPLE_FAILURE"
    NONFINITE_SAMPLE = "ERR_NONFINITE_SAMPLE"

    # --- verification layer ------------------------------------------
    HASH_MISMATCH = "ERR_HASH_MISMATCH"
    TIMELINE_GAP = "ERR_TIMELINE_GAP"
    INDEX_MISS = "ERR_INDEX_MISS"
    INPUT_CHANGED = "ERR_INPUT_CHANGED"


class Severity(str, Enum):
    WARNING = "warning"
    ERROR = "error"
    FATAL = "fatal"


# Damage that a non-strict run is expected to repair (gap or skip).
RECOVERABLE_CODES = frozenset(
    {
        Code.SEGMENT_TRUNCATED,
        Code.PACKET_CORRUPT,
        Code.DECODE_FAILURE,
        Code.REORDERED_REFERENCE,
        Code.DUPLICATE_REFERENCE,
        Code.TIMESTAMP_DISCONTINUITY,
        Code.MISSING_INDEX_ENTRY,
        Code.TIMESTAMP_COLLISION,
    }
)

# Codes that indicate the segment's own media bytes are damaged or absent.
MEDIA_DAMAGE_CODES = frozenset(
    {
        Code.SEGMENT_TRUNCATED,
        Code.PACKET_CORRUPT,
        Code.DECODE_FAILURE,
        Code.REFERENCE_MISSING,
    }
)

_SEVERITY: dict[Code, Severity] = {
    Code.REORDERED_REFERENCE: Severity.WARNING,
    Code.DUPLICATE_REFERENCE: Severity.WARNING,
    Code.TIMESTAMP_COLLISION: Severity.WARNING,
    Code.MISSING_INDEX_ENTRY: Severity.WARNING,
    Code.PACKET_CORRUPT: Severity.ERROR,
    Code.DECODE_FAILURE: Severity.ERROR,
    Code.SEGMENT_TRUNCATED: Severity.ERROR,
    Code.TIMESTAMP_DISCONTINUITY: Severity.ERROR,
    Code.TIMELINE_GAP: Severity.WARNING,
}


def severity_of(code: Code) -> Severity:
    return _SEVERITY.get(code, Severity.FATAL)


@dataclass(frozen=True)
class Locator:
    """Where on the programme a problem occurred.

    ``time_range`` is a tuple ``(start_seconds, end_seconds)`` on the unified
    programme timeline; either bound may be ``None`` when unknown.
    """

    manifest_path: str = ""
    representation: str = ""
    sequence: int | None = None
    time_range: tuple[float | None, float | None] = (None, None)
    segment_ref: str = ""

    def to_json(self) -> dict[str, Any]:
        start, end = self.time_range
        return {
            "manifest_path": self.manifest_path,
            "representation": self.representation,
            "sequence": self.sequence,
            "time_range": [
                None if start is None else round(start, 9),
                None if end is None else round(end, 9),
            ],
            "segment_ref": self.segment_ref,
        }


@dataclass(frozen=True)
class Diagnostic:
    """One structured observation; safe to serialize into the JSON report."""

    code: Code
    message: str
    locator: Locator = field(default_factory=Locator)
    detail: dict[str, Any] = field(default_factory=dict)

    @property
    def severity(self) -> Severity:
        return severity_of(self.code)

    @property
    def recoverable(self) -> bool:
        return self.code in RECOVERABLE_CODES

    def to_json(self) -> dict[str, Any]:
        return {
            "code": self.code.value,
            "severity": self.severity.value,
            "recoverable": self.recoverable,
            "message": self.message,
            "locator": self.locator.to_json(),
            "detail": dict(self.detail),
        }


class RecoveryError(Exception):
    """Base class for errors that carry a full locator."""

    def __init__(
        self,
        code: Code,
        message: str,
        *,
        locator: Locator | None = None,
        detail: dict[str, Any] | None = None,
    ) -> None:
        super().__init__(f"{code.value}: {message}")
        self.code = code
        self.message = message
        self.locator = locator or Locator()
        self.detail = dict(detail or {})

    def diagnostic(self) -> Diagnostic:
        return Diagnostic(self.code, self.message, self.locator, self.detail)

    def to_json(self) -> dict[str, Any]:
        return self.diagnostic().to_json()


class ManifestSyntaxError(RecoveryError):
    """Invalid playlist / MPD syntax or bad CLI parameter (exit code 3)."""


class FatalDamageError(RecoveryError):
    """Damage the active policy says must stop the run (exit code 2)."""


class DiagnosticCollector:
    """Counts codes and keeps a bounded, deterministic sample of diagnostics."""

    def __init__(self, limit: int = 200) -> None:
        self.limit = limit
        self.counts: dict[Code, int] = {}
        self.samples: list[Diagnostic] = []
        # The two pipeline passes re-observe the same decode diagnostics; the
        # signature set keeps the audit trail at one row per distinct finding.
        self._seen: set[tuple[object, ...]] = set()

    @staticmethod
    def _signature(diag: Diagnostic) -> tuple[object, ...]:
        locator = diag.locator
        return (
            diag.code,
            locator.manifest_path,
            locator.representation,
            locator.sequence,
            locator.segment_ref,
            diag.message,
            tuple(sorted(diag.detail.items())),
        )

    def add(self, diag: Diagnostic) -> None:
        signature = self._signature(diag)
        if signature in self._seen:
            return
        self._seen.add(signature)
        self.counts[diag.code] = self.counts.get(diag.code, 0) + 1
        if len(self.samples) < self.limit:
            self.samples.append(diag)

    def add_code(
        self,
        code: Code,
        message: str,
        *,
        locator: Locator | None = None,
        detail: dict[str, Any] | None = None,
    ) -> None:
        self.add(Diagnostic(code, message, locator or Locator(), detail or {}))

    def extend(self, diags: Iterable[Diagnostic]) -> None:
        for diag in diags:
            self.add(diag)

    def count(self, code: Code) -> int:
        return self.counts.get(code, 0)

    @property
    def damaged_segments(self) -> int:
        """Number of distinct segments with actual media or reference damage.

        Padding-only timeline holes (``TIMELINE_GAP``) and hash findings are
        excluded: they are consequences recorded elsewhere, not the source
        damage an archivist reconciles against the cold-storage inventory.
        """
        damage_codes = MEDIA_DAMAGE_CODES
        return len(
            {
                d.locator.sequence
                for d in self.samples
                if d.locator.sequence is not None and d.code in damage_codes
            }
        )

    def to_json(self) -> dict[str, Any]:
        return {
            "codes": {
                code.value: n
                for code, n in sorted(self.counts.items(), key=lambda kv: kv[0].value)
            },
            "samples": [d.to_json() for d in self.samples],
        }


__all__ = [
    "Code",
    "Diagnostic",
    "DiagnosticCollector",
    "FatalDamageError",
    "Locator",
    "ManifestSyntaxError",
    "MEDIA_DAMAGE_CODES",
    "RecoveryError",
    "Severity",
    "RECOVERABLE_CODES",
    "severity_of",
]
