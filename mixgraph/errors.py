"""Project error types and their terminal exit-code contract."""

from __future__ import annotations


class MixgraphError(Exception):
    """Base class for expected, user-facing failures."""

    exit_code = 1

    def __init__(self, message: str, field: str | None = None) -> None:
        super().__init__(message)
        self.message = message
        self.field = field

    def render(self) -> str:
        if self.field:
            return f"{self.field}: {self.message}"
        return self.message


class ManifestError(MixgraphError):
    """Invalid manifest or render parameter."""

    exit_code = 2


class GraphError(MixgraphError):
    """Structurally invalid processing graph."""

    exit_code = 3


class ParityError(MixgraphError):
    """Offline and streamed results differ beyond tolerance."""

    exit_code = 4


class ClipLimitError(MixgraphError):
    """Rendered output exceeds the configured clipping allowance."""

    exit_code = 5

