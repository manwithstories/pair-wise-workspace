"""Detection parameters and their canonical snapshot for idempotent keys."""

from __future__ import annotations

import json
from dataclasses import asdict, dataclass

from .errors import ParamError


@dataclass(frozen=True)
class Params:
    """All values that change the detection result (export margins excluded)."""

    frame_ms: float = 20.0
    hop_ms: float = 10.0
    block_ms: float = 2000.0
    noise_quantile: float = 0.10
    noise_smooth_window: int = 3
    noise_jump_db: float = 6.0
    enter_margin_db: float = 8.0
    exit_margin_db: float = 12.0
    min_silence_ms: float = 150.0
    min_speech_ms: float = 200.0
    fade_start_margin_db: float = 3.0
    fade_tail_max_ms: float = 120.0
    fade_record_ms: float = 400.0
    stable_drop_db: float = 3.0

    def validate(self) -> None:
        if self.frame_ms <= 0 or self.hop_ms <= 0 or self.hop_ms > self.frame_ms:
            raise ParamError("require 0 < hop_ms <= frame_ms")
        if not 0.0 < self.noise_quantile < 1.0:
            raise ParamError("noise_quantile must be within (0, 1)")
        if self.block_ms < self.frame_ms:
            raise ParamError("block_ms must be >= frame_ms")
        for name in ("enter_margin_db", "exit_margin_db", "min_silence_ms",
                     "min_speech_ms", "fade_tail_max_ms"):
            if getattr(self, name) <= 0:
                raise ParamError(f"{name} must be positive")

    def canonical_json(self) -> str:
        """Stable string used together with sha256 as the upsert key."""
        return json.dumps(asdict(self), sort_keys=True, separators=(",", ":"))

    @classmethod
    def from_json(cls, raw: str) -> "Params":
        data = json.loads(raw)
        params = cls(**data)
        params.validate()
        return params
