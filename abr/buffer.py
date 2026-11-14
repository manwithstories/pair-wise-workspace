"""缓冲账本：按下载耗时推进时钟，统计水位、暂停与耗尽。"""

from __future__ import annotations

from dataclasses import dataclass, field


@dataclass
class BufferLedger:
    """播放缓冲的记账器。

    时钟只随 drain/pause 推进；下载耗时由模拟器按实际带宽折算后
    调用 drain 入账。水位归零记一次耗尽事件，归零期间累计耗尽毫秒。
    """

    cap_s: float
    level_s: float = 0.0
    clock_ms: float = 0.0
    rebuffer_events: int = 0
    rebuffer_ms: float = 0.0
    paused_ms: float = 0.0
    _started: bool = field(default=False, repr=False)
    _empty: bool = field(default=False, repr=False)

    def __post_init__(self) -> None:
        if self.cap_s <= 0:
            raise ValueError("cap_s must be positive")

    def drain(self, dt_s: float) -> None:
        """推进时钟并消耗水位（下载进行或暂停期间的真实流逝时间）。"""
        if dt_s <= 0:
            return
        self.clock_ms += dt_s * 1000.0
        if not self._started:
            return  # 启动前的等待计入启动时延，不算耗尽
        if self.level_s >= dt_s:
            self.level_s -= dt_s
            if self.level_s == 0.0:
                self._mark_empty()
        else:
            empty_for = dt_s - self.level_s
            self.level_s = 0.0
            self._mark_empty()
            self.rebuffer_ms += empty_for * 1000.0

    def add_chunk(self, chunk_s: float) -> None:
        """分片下载完成，水位回升（不超过上限），耗尽期结束。"""
        self._started = True
        self.level_s = min(self.cap_s, self.level_s + chunk_s)
        if self.level_s > 0.0:
            self._empty = False

    def should_pause(self) -> bool:
        """水位到上限，暂停拉流。"""
        return self.level_s >= self.cap_s

    def pause(self, dt_s: float) -> None:
        """满缓冲暂停：只推进时钟与消耗水位，记录暂停时长。"""
        self.paused_ms += dt_s * 1000.0
        self.drain(dt_s)

    def _mark_empty(self) -> None:
        if not self._empty:
            self._empty = True
            self.rebuffer_events += 1
