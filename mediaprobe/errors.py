from __future__ import annotations

from dataclasses import dataclass


EXIT_OK = 0
EXIT_BLOCKING = 1
EXIT_IO = 2
EXIT_PARSE = 3
EXIT_INDEX = 4


@dataclass
class MediaError(Exception):
    code: str
    message: str
    path: str | None = None
    offset: int | None = None
    exit_code: int = EXIT_PARSE

    def __str__(self) -> str:
        location = ""
        if self.path:
            location += f" path={self.path}"
        if self.offset is not None:
            location += f" offset={self.offset}"
        return f"{self.code}: {self.message}{location}"

    def to_dict(self) -> dict:
        return {
            "code": self.code,
            "message": self.message,
            "path": self.path,
            "offset": self.offset,
        }


class ContainerError(MediaError):
    pass


class IndexWriteError(MediaError):
    def __init__(self, message: str, *, path: str | None = None, offset: int | None = None):
        super().__init__("INDEX_WRITE_FAILED", message, path, offset, EXIT_INDEX)
