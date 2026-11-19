"""Error model shared by probing, recovery, mapping and remuxing.

Every error carries a :class:`MediaLocation` so a broadcast engineer can jump
straight to the asset, stream and (when known) byte offset / timestamp that
caused the problem.  The exception classes are deliberately split into
*recoverable*, *skippable* and *fatal* families; the recovery planner catches
the first two per packet/stream, while the batch runner maps fatal errors to
the process exit codes documented in :mod:`bcastqc.cli`.
"""

from __future__ import annotations

from dataclasses import dataclass
from fractions import Fraction
from typing import Any

EXIT_OK = 0
EXIT_THRESHOLD = 1
EXIT_USAGE = 2
EXIT_MEDIA = 3


@dataclass(frozen=True, slots=True)
class MediaLocation:
    """Where a defect lives inside a delivery asset."""

    asset: str | None = None
    stream: int | None = None
    packet_pos: int | None = None
    timestamp: Fraction | None = None
    rule: int | None = None
    detail: str | None = None

    def to_dict(self) -> dict[str, Any]:
        out: dict[str, Any] = {}
        if self.asset is not None:
            out["asset"] = self.asset
        if self.stream is not None:
            out["stream"] = self.stream
        if self.packet_pos is not None:
            out["packet_pos"] = self.packet_pos
        if self.timestamp is not None:
            out["timestamp"] = str(self.timestamp)
            out["timestamp_s"] = round(float(self.timestamp), 6)
        if self.rule is not None:
            out["rule"] = self.rule
        if self.detail is not None:
            out["detail"] = self.detail
        return out


class QCError(Exception):
    """Base class for all bcastqc failures."""

    code = "qc_error"
    exit_code = EXIT_MEDIA
    severity = "error"
    recoverable = False

    def __init__(
        self,
        message: str,
        *,
        location: MediaLocation | None = None,
        code: str | None = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.location = location or MediaLocation()
        if code is not None:
            self.code = code

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "severity": self.severity,
            "message": self.message,
            "recoverable": self.recoverable,
            "location": self.location.to_dict(),
        }

    def __str__(self) -> str:  # pragma: no cover - cosmetic
        loc = self.location.to_dict()
        if loc:
            return f"{self.message} [{', '.join(f'{k}={v}' for k, v in loc.items())}]"
        return self.message


class RecoverableError(QCError):
    """A defect the recovery planner can work around (packet stays locatable)."""

    exit_code = EXIT_THRESHOLD
    severity = "warning"
    recoverable = True
    code = "recoverable_defect"


class SkipError(RecoverableError):
    """An item that must be skipped explicitly (corrupt / undecodable packet)."""

    code = "skipped"


class FatalError(QCError):
    """A defect that aborts the current asset."""

    code = "fatal"


class UsageError(FatalError):
    """Bad CLI arguments, limits or missing paths."""

    exit_code = EXIT_USAGE
    code = "usage_error"


class InputError(UsageError):
    """The input cannot be accepted at all (size, track count, extension...)."""

    code = "input_error"


class UnrecoverableMediaError(FatalError):
    """Media is damaged beyond the recovery policy (no usable streams)."""

    code = "unrecoverable_media"


class LosslessCopyRefusedError(FatalError):
    """The input combination cannot be copied losslessly into the container."""

    code = "lossless_copy_refused"


class AssetTimeoutError(UnrecoverableMediaError):
    """A single asset exceeded the configured processing budget."""

    code = "asset_timeout"


def errors_to_dict(errors: list[QCError]) -> list[dict[str, Any]]:
    return [err.to_dict() for err in errors]
