"""Verification records: per-check results and the aggregate report."""

from __future__ import annotations

from dataclasses import dataclass, field
from pathlib import Path


@dataclass
class CheckResult:
    name: str
    ok: bool
    detail: str = ""

    def as_dict(self) -> dict[str, object]:
        return {"name": self.name, "ok": self.ok, "detail": self.detail}


@dataclass
class VerifyReport:
    path: Path
    sha256: str
    size: int
    checks: list[CheckResult] = field(default_factory=list)
    track_summaries: list[dict[str, object]] = field(default_factory=list)
    box_count: int = 0

    @property
    def ok(self) -> bool:
        return all(check.ok for check in self.checks)

    @property
    def failures(self) -> list[CheckResult]:
        return [check for check in self.checks if not check.ok]

    def as_dict(self) -> dict[str, object]:
        return {
            "path": str(self.path),
            "size": self.size,
            "sha256": self.sha256,
            "ok": self.ok,
            "box_count": self.box_count,
            "checks": [check.as_dict() for check in self.checks],
            "failure_count": len(self.failures),
            "tracks": list(self.track_summaries),
        }


__all__ = ["CheckResult", "VerifyReport"]
