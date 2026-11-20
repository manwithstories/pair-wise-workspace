"""Locatable, recoverable error model.

Every failure the pipeline can produce is expressed as a :class:`MediaError`
subclass carrying a machine-readable ``code``, a severity, and a structured
``context`` dict with as much locating information as is available
(track id, packet ordinal, timestamp, byte offset, node id ...).

The design goal is that an operator reading a failure can answer three
questions without rerunning the tool:

* *where* did it happen (track / packet / frame / byte offset),
* *how far off* were our estimates (``locator_error_frames``,
  ``locator_error_us``),
* whether the run can continue (``recoverable``) and, if it did continue,
  what policy was applied (``strategy``).

Errors also serialise to JSON so the CLI can emit them next to the summary
report and return a distinguishable exit code.
"""

from __future__ import annotations

from dataclasses import dataclass, field
from enum import Enum
from typing import Any, Mapping

__all__ = [
    "Severity",
    "MediaError",
    "ParameterError",
    "TimelineError",
    "ProbeError",
    "AudioError",
    "GraphError",
    "OutputError",
    "CheckpointError",
    "error_from_dict",
    "EXIT_OK",
    "EXIT_USAGE",
    "EXIT_GRAPH",
    "EXIT_OUTPUT",
    "EXIT_RECOVERED",
]

# Exit codes. 0 and 2 are mandated by the acceptance contract; the rest are
# distinct so callers can programmatically separate failure classes.
EXIT_OK = 0
EXIT_USAGE = 1  # bad arguments / input validation
EXIT_RECOVERED = 2  # input was damaged but policy recovered and reported
EXIT_GRAPH = 3  # dependency graph or checkpoint validation failure
EXIT_OUTPUT = 4  # output verification (hash / drift / count) failure


class Severity(str, Enum):
    INFO = "info"
    WARNING = "warning"
    ERROR = "error"
    FATAL = "fatal"


@dataclass(eq=False)
class MediaError(Exception):
    """Base class for every locatable mediacore failure."""

    message: str
    code: str = "media_error"
    severity: Severity = Severity.ERROR
    recoverable: bool = False
    context: dict[str, Any] = field(default_factory=dict)

    def __post_init__(self) -> None:
        super().__init__(self.message)

    # -- locating helpers ---------------------------------------------------
    def with_context(self, **kw: Any) -> "MediaError":
        """Return ``self`` after merging extra locating context."""
        self.context.update({k: v for k, v in kw.items() if v is not None})
        return self

    def to_dict(self) -> dict[str, Any]:
        return {
            "code": self.code,
            "kind": type(self).__name__,
            "severity": self.severity.value,
            "recoverable": self.recoverable,
            "message": self.message,
            "context": _jsonable(self.context),
        }

    @classmethod
    def from_dict(cls, data: Mapping[str, Any]) -> "MediaError":
        return error_from_dict(data)


class ParameterError(MediaError):
    """Invalid CLI/API parameter; never recoverable."""

    def __init__(self, message: str, **ctx: Any) -> None:
        super().__init__(message, code="parameter", severity=Severity.FATAL, context=ctx)


class TimelineError(MediaError):
    """Bad clip, transition, overlap or empty/out-of-range segment."""

    def __init__(
        self,
        message: str,
        *,
        code: str = "timeline",
        recoverable: bool = False,
        severity: Severity = Severity.ERROR,
        **ctx: Any,
    ) -> None:
        super().__init__(
            message, code=code, severity=severity, recoverable=recoverable, context=ctx
        )


class ProbeError(MediaError):
    """Damage detected while scanning media: truncation, corruption, gaps."""

    def __init__(
        self,
        message: str,
        *,
        code: str = "probe",
        recoverable: bool = True,
        severity: Severity = Severity.ERROR,
        **ctx: Any,
    ) -> None:
        super().__init__(
            message, code=code, severity=severity, recoverable=recoverable, context=ctx
        )


class AudioError(MediaError):
    """Audio conversion / loudness failure."""

    def __init__(self, message: str, *, code: str = "audio", **ctx: Any) -> None:
        super().__init__(message, code=code, severity=Severity.ERROR, context=ctx)


class GraphError(MediaError):
    """Cyclic dependency or unsatisfiable graph."""

    def __init__(self, message: str, *, code: str = "graph", **ctx: Any) -> None:
        super().__init__(message, code=code, severity=Severity.FATAL, context=ctx)


class OutputError(MediaError):
    """Output verification failure (hash mismatch, drift, wrong count)."""

    def __init__(self, message: str, *, code: str = "output", **ctx: Any) -> None:
        super().__init__(message, code=code, severity=Severity.FATAL, context=ctx)


class CheckpointError(MediaError):
    """Checkpoint unreadable or inconsistent with current inputs."""

    def __init__(
        self, message: str, *, code: str = "checkpoint", recoverable: bool = True, **ctx: Any
    ) -> None:
        super().__init__(
            message, code=code, severity=Severity.WARNING, recoverable=recoverable, context=ctx
        )


_CLASSES: dict[str, type[MediaError]] = {
    "MediaError": MediaError,
    "ParameterError": ParameterError,
    "TimelineError": TimelineError,
    "ProbeError": ProbeError,
    "AudioError": AudioError,
    "GraphError": GraphError,
    "OutputError": OutputError,
    "CheckpointError": CheckpointError,
}


def error_from_dict(data: Mapping[str, Any]) -> MediaError:
    """Rebuild an error from :meth:`MediaError.to_dict` output."""
    kind = str(data.get("kind", "MediaError"))
    cls = _CLASSES.get(kind, MediaError)
    err = cls.__new__(cls)
    MediaError.__init__(
        err,
        str(data.get("message", "")),
        code=str(data.get("code", "media_error")),
        severity=Severity(data.get("severity", "error")),
        recoverable=bool(data.get("recoverable", False)),
        context=dict(data.get("context") or {}),
    )
    return err


def _jsonable(value: Any) -> Any:
    if isinstance(value, dict):
        return {str(k): _jsonable(v) for k, v in value.items()}
    if isinstance(value, (list, tuple, set)):
        return [_jsonable(v) for v in value]
    if isinstance(value, Severity):
        return value.value
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    return repr(value)
