"""Error contract shared by every module and the CLI exit codes."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any


EXIT_OK = 0
EXIT_ARGUMENT = 2
EXIT_FORMAT = 3
EXIT_MISMATCH = 4


@dataclass
class MixerError(Exception):
    """A failure that maps to a documented exit code and a stable error code.

    ``code`` is a short machine token (``invalid_rate``), ``message`` is the
    human sentence, and ``track`` names the offending track whenever the
    failure can be attributed to one.
    """

    code: str
    message: str
    exit_code: int = EXIT_ARGUMENT
    track: str | None = None
    details: dict[str, Any] = field(default_factory=dict)

    def __str__(self) -> str:  # pragma: no cover - trivial
        prefix = f"[{self.track}] " if self.track else ""
        return f"{prefix}{self.message}"

    def to_dict(self) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "code": self.code,
            "message": self.message,
            "exit_code": self.exit_code,
        }
        if self.track is not None:
            payload["track"] = self.track
        if self.details:
            payload["details"] = self.details
        return payload


def argument_error(code: str, message: str, **kw: Any) -> MixerError:
    return MixerError(code, message, EXIT_ARGUMENT, **kw)


def format_error(code: str, message: str, **kw: Any) -> MixerError:
    return MixerError(code, message, EXIT_FORMAT, **kw)


def mismatch_error(code: str, message: str, **kw: Any) -> MixerError:
    return MixerError(code, message, EXIT_MISMATCH, **kw)
