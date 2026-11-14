"""Exception types mapped to the CLI exit codes in the product spec."""


class MkvToolError(Exception):
    exit_code = 1


class InputParseError(MkvToolError):
    """The input is not a readable EBML/Matroska prefix."""

    exit_code = 2


class UnsupportedTrackError(MkvToolError):
    """No requested track can be represented by the local MP4 writer."""

    exit_code = 3


class TimelineError(MkvToolError):
    """Timestamps cannot be repaired without ambiguity."""

    exit_code = 4


class VerificationError(MkvToolError):
    """The generated MP4 disagrees with the source timeline."""

    exit_code = 5
