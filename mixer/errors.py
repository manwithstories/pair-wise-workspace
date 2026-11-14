"""Structured exceptions and CLI exit-code mapping."""

from __future__ import annotations

from typing import Any


EXIT_MANIFEST = 2
EXIT_RENDER = 3
EXIT_IO = 4


class MixerError(Exception):
    """An error safe to expose as one-line JSON from the CLI."""

    code = EXIT_RENDER
    kind = "render"

    def __init__(
        self,
        message: str,
        *,
        details: dict[str, Any] | None = None,
        code: int | None = None,
        kind: str | None = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        self.details = details or {}
        if code is not None:
            self.code = code
        if kind is not None:
            self.kind = kind

    def payload(self) -> dict[str, Any]:
        return {
            "error": {
                "code": self.code,
                "type": self.kind,
                "message": self.message,
                "details": self.details,
            }
        }


class ManifestError(MixerError):
    code = EXIT_MANIFEST
    kind = "manifest"


class GraphError(MixerError):
    code = EXIT_MANIFEST
    kind = "graph"


class RenderError(MixerError):
    code = EXIT_RENDER
    kind = "render"


class ParityError(MixerError):
    code = EXIT_RENDER
    kind = "parity"


class MixerIOError(MixerError):
    code = EXIT_IO
    kind = "io"
