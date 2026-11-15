"""Typed errors; each maps to a CLI exit code."""


class Mkv2Mp4Error(Exception):
    """Base error; ``exit_code`` is returned by the CLI."""

    exit_code = 1


class EbmlError(Mkv2Mp4Error):
    """Malformed EBML/MKV structure."""

    exit_code = 3

    def __init__(self, message, offset=None):
        self.offset = offset
        if offset is not None:
            message = "%s (offset 0x%x)" % (message, offset)
        super().__init__(message)


class UnsupportedCodecError(Mkv2Mp4Error):
    """Track codec is not AVC/AAC and cannot be remuxed."""

    exit_code = 4


class TimestampError(Mkv2Mp4Error):
    """Timestamps cannot be expressed in the target timescale."""

    exit_code = 5


class VerifyError(Mkv2Mp4Error):
    """Post-remux verification failed."""

    exit_code = 6
