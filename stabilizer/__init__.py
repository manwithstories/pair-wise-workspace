"""Pure-local video stabilization toolkit.

Pipeline: frame input -> feature detection -> tracking -> robust motion
estimation -> rolling-shutter correction -> trajectory smoothing ->
stabilized frame export -> JSON metrics.
"""

__version__ = "0.1.0"

EXIT_OK = 0
EXIT_USAGE = 2
EXIT_INSUFFICIENT_TRACKS = 3
EXIT_NUMERICAL = 4


class StabilizerError(Exception):
    """Base error carrying a process exit code and diagnostics."""

    exit_code = EXIT_NUMERICAL

    def __init__(self, message, diagnostics=None):
        super().__init__(message)
        self.diagnostics = diagnostics or {}


class InputError(StabilizerError):
    """Bad parameters or bad input data (exit 2)."""

    exit_code = EXIT_USAGE


class InsufficientTracksError(StabilizerError):
    """Too few valid tracked points (exit 3)."""

    exit_code = EXIT_INSUFFICIENT_TRACKS


class NumericalError(StabilizerError):
    """Singular system / NaN / export failure (exit 4)."""

    exit_code = EXIT_NUMERICAL
