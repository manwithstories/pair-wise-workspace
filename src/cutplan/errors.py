from __future__ import annotations


class CutplanError(Exception):
    def __init__(self, code: str, message: str = "", exit_code: int = 2) -> None:
        super().__init__(message or code)
        self.code = code
        self.message = message or code
        self.exit_code = exit_code

    def as_dict(self) -> dict[str, str]:
        return {"code": self.code, "message": self.message}
