"""Project-specific exceptions carrying stable process exit codes."""


class FrameAuditError(Exception):
    """Base class for expected command failures."""


class InputError(FrameAuditError):
    """Invalid command input or unusable source container."""


class ProcessingError(FrameAuditError):
    """Decode or output failure that prevented a complete result."""


class VerifyError(FrameAuditError):
    """Index and proxy outputs disagree."""

