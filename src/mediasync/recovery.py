"""检查点恢复协调：摘要校验、隔离与安全边界确定。

恢复时先清理中断残留的未确认临时块，再校验输入摘要、配置摘要与
已确认块校验和；任一不匹配则把旧检查点整体隔离，从块 0 重算。
"""

from __future__ import annotations

from pathlib import Path

from .checkpoint import CheckpointStore
from .digest import read_json, write_json
from .errors import ErrorBook, ErrorCode


class RecoveryCoordinator:
    def __init__(self, store: CheckpointStore, workdir: str, errors: ErrorBook) -> None:
        self.store = store
        self.workdir = workdir
        self.errors = errors
        self.quarantine_note = ""

    def prepare_resume(self, input_sum: dict, cfg_sum: str) -> int:
        """返回应从第几块开始；无效检查点隔离后返回 0。"""
        meta_path = Path(self.store.dir) / "job.json"
        if not meta_path.exists():
            write_json(
                "checkpoint/job.json", self.workdir,
                {"input_digest": input_sum, "config_digest": cfg_sum},
            )
            return 0
        try:
            self.store.verify_blocks()
        except ValueError:
            self._quarantine("已确认块校验和不匹配")
            return 0
        previous = read_json("checkpoint/job.json", self.workdir)
        if (
            previous.get("input_digest") != input_sum
            or previous.get("config_digest") != cfg_sum
        ):
            self._quarantine("输入摘要或配置摘要不匹配")
            return 0
        return self.store.committed_count()

    def _quarantine(self, reason: str) -> None:
        dest = self.store.quarantine(reason)
        self.quarantine_note = str(dest)
        write_json(
            "checkpoint/job.json", self.workdir,
            {"input_digest": None, "config_digest": None},
        )
        # 清空（含 verify 阶段的致命错误），再保留一条非致命隔离留痕。
        self.errors.clear()
        self.errors.add(
            ErrorCode.CHECKPOINT_SUMMARY_MISMATCH,
            f"旧检查点已隔离: {reason}",
            source=str(dest),
            fatal=False,
        )
