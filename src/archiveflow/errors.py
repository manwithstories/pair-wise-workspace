"""Shared exception types for the archiveflow pipeline."""


class ArchiveflowError(Exception):
    """Base class for all archiveflow errors."""


class UnrecoverableError(ArchiveflowError):
    """Input cannot be recovered into usable material (exit status 1)."""


class CapabilityError(ArchiveflowError):
    """A required codec/container capability is missing (exit status 1)."""


class ParameterError(ArchiveflowError):
    """Invalid run parameters (exit status 2)."""


class CancelledError(ArchiveflowError):
    """Pipeline stage was cancelled before completion."""
