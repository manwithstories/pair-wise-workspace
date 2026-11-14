"""码率决策器：吞吐上界叠加缓冲约束。

规则（阈值均可由构造参数覆盖，供 sweep 网格搜索）：
  - 升档：估计值连续 up_consec 段高于目标码率 up_factor 倍，且水位 >= min_buffer_up_s
  - 紧急降档：估计值低于当前码率 down_factor 倍，或水位低于 panic_buffer_s；
    直接落到估计值可支撑的档位（至少降一级），避免长下载抽干缓冲
  - 同档往返：距上次换档不足 cooldown_segments 段时不允许升档
  - 启动阶段：前 startup_segments 段取最低档，除非估计值超过次高档 startup_factor 倍
"""

from __future__ import annotations

from dataclasses import dataclass

from .ladder import Ladder


@dataclass
class DeciderConfig:
    up_factor: float = 1.15
    up_consec: int = 2
    min_buffer_up_s: float = 8.0
    down_factor: float = 0.8
    panic_buffer_s: float = 5.0
    cooldown_segments: int = 3
    startup_segments: int = 2
    startup_factor: float = 2.0


class Decider:
    def __init__(self, ladder: Ladder, config: DeciderConfig | None = None) -> None:
        self._ladder = ladder
        self._cfg = config or DeciderConfig()
        self._idx = 0
        self._seg = 0
        self._up_streak = 0
        self._last_switch_seg = -10**9

    @property
    def current_index(self) -> int:
        return self._idx

    def decide(self, estimate_kbps: float, buffer_s: float) -> int:
        cfg = self._cfg
        bitrates = self._ladder.bitrates_kbps
        current = bitrates[self._idx]

        if self._seg < cfg.startup_segments:
            chosen = self._startup_choice(estimate_kbps)
        elif estimate_kbps < cfg.down_factor * current or buffer_s < cfg.panic_buffer_s:
            chosen = self._emergency_choice(estimate_kbps)
        else:
            chosen = self._try_upgrade(estimate_kbps, buffer_s)

        if chosen != self._idx:
            self._last_switch_seg = self._seg
            self._up_streak = 0
        self._idx = chosen
        self._seg += 1
        return chosen

    def _startup_choice(self, estimate_kbps: float) -> int:
        bitrates = self._ladder.bitrates_kbps
        if len(bitrates) >= 2 and estimate_kbps > self._cfg.startup_factor * bitrates[-2]:
            return len(bitrates) - 1
        return 0

    def _emergency_choice(self, estimate_kbps: float) -> int:
        bitrates = self._ladder.bitrates_kbps
        fit = 0
        for i, b in enumerate(bitrates):
            if b <= estimate_kbps:
                fit = i
        return min(self._idx - 1, fit) if self._idx > 0 else 0

    def _try_upgrade(self, estimate_kbps: float, buffer_s: float) -> int:
        cfg = self._cfg
        bitrates = self._ladder.bitrates_kbps
        if self._idx >= len(bitrates) - 1:
            return self._idx
        target = bitrates[self._idx + 1]
        if estimate_kbps > cfg.up_factor * target and buffer_s >= cfg.min_buffer_up_s:
            self._up_streak += 1
        else:
            self._up_streak = 0
        cooled_down = self._seg - self._last_switch_seg >= cfg.cooldown_segments
        if self._up_streak >= cfg.up_consec and cooled_down:
            return self._idx + 1
        return self._idx
