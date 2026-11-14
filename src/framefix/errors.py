"""Exception types shared by the CLI and pipeline modules."""


class FramefixError(Exception):
    """Base class for expected user-facing failures."""


class InputOpenError(FramefixError):
    """The path cannot be opened as a media container."""


class NoVideoStreamError(FramefixError):
    """The container has no video stream."""


class UnsupportedCodecError(FramefixError):
    """The video stream is not H.264 or H.265."""


class DecodeError(FramefixError):
    """Decoding failed after zero or more frames were produced."""

    def __init__(self, message: str, frames_emitted: int = 0):
        super().__init__(message)
        self.frames_emitted = frames_emitted


class RepairError(FramefixError):
    """A repair plan cannot be applied safely."""

