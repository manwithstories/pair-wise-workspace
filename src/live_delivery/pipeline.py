"""有界队列流水线：串联阶段并传播背压、取消和错误状态。

内存占用随队列上限（条数 × 字节数）增长，与输入总时长无关。
"""
from __future__ import annotations

import queue
import threading
from typing import Any, Callable

from .errors import MediaError, PipelineError

SENTINEL = object()  # 阶段结束标记


class BoundedQueue:
    """条数与字节数双上限队列；满时阻塞上游形成背压，关闭后写入失败。"""

    def __init__(self, max_items: int, max_bytes: int) -> None:
        self._q: queue.Queue = queue.Queue(maxsize=max_items)
        self._max_bytes = max(1, max_bytes)
        self._cond = threading.Condition()
        self._current = 0
        self.peak_bytes = 0
        self._closed = False

    def put(self, item: Any, nbytes: int = 0) -> bool:
        with self._cond:
            # 单块超过上限时允许独占通过，避免死锁
            while (not self._closed and self._current > 0
                   and self._current + nbytes > self._max_bytes):
                self._cond.wait(timeout=0.1)
            if self._closed:
                return False
            self._current += nbytes
            self.peak_bytes = max(self.peak_bytes, self._current)
        while not self._closed:
            try:
                self._q.put((item, nbytes), timeout=0.1)
                return True
            except queue.Full:
                continue
        with self._cond:  # 关闭时未入队，退回字节额度
            self._current -= nbytes
            self._cond.notify_all()
        return False

    def get(self) -> Any:
        """返回下一个条目；超时返回 None（调用方检查取消状态）。"""
        try:
            item, nbytes = self._q.get(timeout=0.1)
        except queue.Empty:
            return None
        if nbytes:
            with self._cond:
                self._current -= nbytes
                self._cond.notify_all()
        return item

    def close(self) -> None:
        with self._cond:
            self._closed = True
            self._cond.notify_all()


class Pipeline:
    """线程化阶段流水线：任一阶段失败即取消全局并释放队列。"""

    def __init__(self) -> None:
        self.errors: list[MediaError] = []
        self.cancelled = threading.Event()
        self._threads: list[threading.Thread] = []
        self._queues: list[BoundedQueue] = []

    def queue(self, max_items: int, max_bytes: int) -> BoundedQueue:
        q = BoundedQueue(max_items, max_bytes)
        self._queues.append(q)
        return q

    def add_stage(self, name: str, fn: Callable[[], None]) -> None:
        def runner() -> None:
            try:
                fn()
            except Exception as exc:  # noqa: BLE001 —— 阶段错误统一记录
                self.errors.append(MediaError(
                    "pipeline", f"阶段 {name} 失败: {exc}", recoverable=False))
                self.cancelled.set()
                for q in self._queues:
                    q.close()

        self._threads.append(
            threading.Thread(target=runner, name=f"live-delivery-{name}", daemon=True)
        )

    @property
    def peak_buffer_bytes(self) -> int:
        return sum(q.peak_bytes for q in self._queues)

    def run(self) -> None:
        for t in self._threads:
            t.start()
        for t in self._threads:
            t.join()
        if self.errors:
            raise PipelineError(self.errors[0].message)
