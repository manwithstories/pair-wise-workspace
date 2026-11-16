"""Bitrate selection policy.

The policy consumes a :class:`Estimate` (never raw samples) together with
the player :class:`BufferState` and returns the next ladder index.

Design goals, mapped to the acceptance criteria:

* **fast downshift** — a segment that no longer fits the safe estimate
  drops immediately to the highest affordable level (one configurable
  confirmation, default 1); a thin buffer pre-empts even harder.
* **confirmed upshift** — a higher level is only taken after
  ``upshift_confirm`` consecutive segments where it fits with margin;
  any failure resets the streak, so a flapping recovery cannot oscillate.
* **buffer water levels** — low buffer forces the floor level, high buffer
  is required before any upshift is even considered.
* **never leave the ladder** — every returned index is clamped.
"""

from __future__ import annotations

from dataclasses import dataclass

from .models import ABRConfigError, BitrateLadder, BufferState, Estimate


@dataclass(frozen=True, slots=True)
class PolicyConfig:
    """Knobs governing up/down-shift behaviour."""

    upshift_confirm: int = 3
    downshift_confirm: int = 1
    safety_margin: float = 0.85
    low_buffer_ratio: float = 0.20
    upshift_buffer_ratio: float = 0.35

    def __post_init__(self) -> None:
        if self.upshift_confirm < 1:
            raise ABRConfigError("upshift-confirm must be >= 1")
        if self.downshift_confirm < 1:
            raise ABRConfigError("downshift-confirm must be >= 1")
        if not 0.0 < self.safety_margin <= 1.0:
            raise ABRConfigError("safety-margin must be in (0, 1]")
        if not 0.0 <= self.low_buffer_ratio < 1.0:
            raise ABRConfigError("low-buffer-ratio must be in [0, 1)")
        if not 0.0 <= self.upshift_buffer_ratio <= 1.0:
            raise ABRConfigError("upshift-buffer-ratio must be in [0, 1]")
        if self.low_buffer_ratio >= self.upshift_buffer_ratio:
            raise ABRConfigError(
                "low-buffer-ratio must be below upshift-buffer-ratio"
            )


class ABRPolicy:
    """Stateful ladder selector; one instance per simulation."""

    def __init__(self, ladder: BitrateLadder, config: PolicyConfig) -> None:
        self.ladder = ladder
        self.config = config
        self._upshift_streak = 0
        self._downshift_streak = 0

    # ------------------------------------------------------------------ #
    def select(
        self,
        current_index: int,
        estimate: Estimate,
        buffer_state: BufferState,
    ) -> int:
        """Return the ladder index to use for the next segment."""
        current_index = self.ladder.clamp(current_index)

        # 1. Any unsafe estimate is treated conservatively: no upshift,
        #    hold the floor while the link is unknown or dead.
        if not estimate.state.safe:
            self._upshift_streak = 0
            self._downshift_streak = 0
            return 0

        effective = estimate.bandwidth_kbps * self.config.safety_margin
        affordable = self.ladder.highest_affordable(effective)
        current_bitrate = self.ladder.bitrates[current_index]
        cfg = self.config

        # 2. A drained buffer always wins: protect playback first. When the
        #    water level is at/below the low mark, jump straight to the floor.
        if buffer_state.ratio <= cfg.low_buffer_ratio:
            self._upshift_streak = 0
            return 0

        # 3. Fast downshift when the current level no longer fits.
        if current_bitrate > effective:
            self._upshift_streak = 0
            self._downshift_streak += 1
            if self._downshift_streak >= cfg.downshift_confirm:
                self._downshift_streak = 0
                return self.ladder.clamp(min(current_index, affordable))
            return current_index
        self._downshift_streak = 0

        # 4. Upshift only with a healthy buffer AND a confirmed streak.
        if affordable <= current_index:
            self._upshift_streak = 0
            return current_index
        if buffer_state.ratio < cfg.upshift_buffer_ratio:
            self._upshift_streak = 0
            return current_index

        self._upshift_streak += 1
        if self._upshift_streak >= cfg.upshift_confirm:
            self._upshift_streak = 0
            # Climb one rung at a time even if the estimate jumps far.
            return self.ladder.clamp(current_index + 1)
        return current_index

    # ------------------------------------------------------------------ #
    @property
    def upshift_streak(self) -> int:
        return self._upshift_streak

    def reset(self) -> None:
        self._upshift_streak = 0
        self._downshift_streak = 0
