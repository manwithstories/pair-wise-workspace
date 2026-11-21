"""有界生产者—消费者队列与背压。

同时受 *字节上限* 与 *包数上限* 约束：任一触顶即视为满。
消费者暂停/限速时，生产者在条件变量上阻塞（计入阻塞时长），
或按 ABORT 策略抛出 :class:`QueueFullAborted`，绝不无界缓存。
内存占用 = 队列内全部包负载之和 + 在途包，不随输入时长增长。
"""
from __future__ import annotations

import threading
import time
from collections import deque
from dataclasses import dataclass
from enum import Enum

from .packet import PacketRecord


class BackpressurePolicy(str, Enum):
    BLOCK = "block"   # 队列满时生产者阻塞等待
    ABORT = "abort"   # 队列满时立即取消（抛出 QueueFullAborted）


class QueueFullAborted(RuntimeError):
    """ABORT 策略下队列已满，生产者被取消。"""


@dataclass(slots=True)
class QueueStats:
    enqueued: int = 0
    dequeued: int = 0
    dropped: int = 0
    peak_packets: int = 0
    peak_bytes: int = 0
    block_seconds: float = 0.0
    block_count: int = 0

    def to_dict(self) -> dict:
        return {
            "enqueued": self.enqueued,
            "dequeued": self.dequeued,
            "dropped": self.dropped,
            "queue_peak_packets": self.peak_packets,
            "queue_peak_bytes": self.peak_bytes,
            "block_seconds": round(self.block_seconds, 6),
            "block_count": self.block_count,
        }


class BoundedQueue:
    """线程安全的双上限 FIFO 队列。"""

    def __init__(
        self,
        max_bytes: int,
        max_packets: int | None = None,
        policy: BackpressurePolicy = BackpressurePolicy.BLOCK,
    ) -> None:
        if max_bytes <= 0:
            raise ValueError("max_bytes 必须为正数")
        if max_packets is not None and max_packets <= 0:
            raise ValueError("max_packets 必须为正数或 None")
        self._max_bytes = max_bytes
        self._max_packets = max_packets
        self._policy = policy
        self._items: deque[PacketRecord] = deque()
        self._cur_bytes = 0
        self._cond = threading.Condition()
        self._closed = False
        self.stats = QueueStats()

    @property
    def max_bytes(self) -> int:
        return self._max_bytes

    def _full(self, incoming_size: int) -> bool:
        if self._cur_bytes + incoming_size > self._max_bytes:
            return True
        return (
            self._max_packets is not None
            and len(self._items) + 1 > self._max_packets
        )

    def put(self, record: PacketRecord) -> None:
        """入队；满时按策略阻塞或取消。close 后再放入抛 RuntimeError。"""
        with self._cond:
            if self._closed:
                raise RuntimeError("队列已关闭，拒绝新包")
            if self._full(record.size):
                if self._policy is BackpressurePolicy.ABORT:
                    self.stats.dropped += 1
                    raise QueueFullAborted(
                        f"队列已满（{self._cur_bytes} 字节 / "
                        f"{len(self._items)} 包），ABORT 策略取消生产"
                    )
                started = time.monotonic()
                self.stats.block_count += 1
                while not self._closed and self._full(record.size):
                    self._cond.wait()
                self.stats.block_seconds += time.monotonic() - started
                if self._closed:
                    raise RuntimeError("等待期间队列被关闭")
            self._items.append(record)
            self._cur_bytes += record.size
            self.stats.enqueued += 1
            self.stats.peak_packets = max(self.stats.peak_packets, len(self._items))
            self.stats.peak_bytes = max(self.stats.peak_bytes, self._cur_bytes)
            self._cond.notify_all()

    def get(self, timeout: float | None = None) -> PacketRecord | None:
        """出队；空且已关闭返回 None，空且未关闭在 timeout 内等待。"""
        with self._cond:
            if not self._items:
                if self._closed:
                    return None
                self._cond.wait(timeout)
                if not self._items:
                    return None
            record = self._items.popleft()
            self._cur_bytes -= record.size
            self.stats.dequeued += 1
            self._cond.notify_all()
            return record

    def close(self) -> None:
        with self._cond:
            self._closed = True
            self._cond.notify_all()

    def qsize(self) -> tuple[int, int]:
        with self._cond:
            return len(self._items), self._cur_bytes
