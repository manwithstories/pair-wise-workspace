"""Exception hierarchy mapped to the CLI exit-code contract."""


class TalkmapError(Exception):
    """Base error; default exit code 2 (parameter error)."""

    exit_code = 2


class ParamError(TalkmapError):
    """Invalid CLI arguments or parameters."""

    exit_code = 2


class AudioReadError(TalkmapError):
    """WAV container or PCM data cannot be decoded."""

    exit_code = 3


class DatabaseError(TalkmapError):
    """sqlite state cannot be read or written."""

    exit_code = 4


class QueryEmpty(TalkmapError):
    """A filtered query matched no segments."""

    exit_code = 1
