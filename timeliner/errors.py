"""Typed errors carrying CLI exit codes."""


class TimelinerError(Exception):
    exit_code = 1

    def __init__(self, message, location=None):
        self.location = location
        if location:
            message = "%s: %s" % (location, message)
        super().__init__(message)


class TimelineError(TimelinerError):
    """Invalid arguments or illegal timeline document."""

    exit_code = 2


class AssetError(TimelinerError):
    """Missing, corrupt, or size-mismatched asset."""

    exit_code = 3


class DigestMismatchError(TimelinerError):
    """Frame digest recomputation disagrees with the manifest."""

    exit_code = 4


class AVAlignError(TimelinerError):
    """Audio/video alignment error exceeds the allowed threshold."""

    exit_code = 5


class BlendModeError(TimelinerError):
    """Unknown blend mode."""

    exit_code = 6
