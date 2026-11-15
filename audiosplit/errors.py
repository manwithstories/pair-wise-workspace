"""Application errors and stable process exit codes."""

from __future__ import annotations


EXIT_OK = 0
EXIT_USAGE = 2
EXIT_AUDIO = 3
EXIT_SELFTEST = 4
EXIT_DATABASE = 5


class AppError(Exception):
    exit_code = EXIT_AUDIO

    def __init__(self, code: str, message: str, *, path: str | None = None):
        super().__init__(message)
        self.code = code
        self.message = message
        self.path = path

    def as_dict(self) -> dict:
        payload = {"code": self.code, "message": self.message}
        if self.path is not None:
            payload["path"] = self.path
        return payload


class UsageError(AppError):
    exit_code = EXIT_USAGE


class AudioError(AppError):
    exit_code = EXIT_AUDIO


class FileSystemError(AudioError):
    pass


class DatabaseError(AppError):
    exit_code = EXIT_DATABASE


class SelftestError(AppError):
    exit_code = EXIT_SELFTEST

