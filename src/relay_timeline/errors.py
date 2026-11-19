"""可定位错误模型：所有模块共享的异常类型与进程退出码。

退出码约定（cli 层统一使用）：
    0  验收通过
    2  输入或清单无效（时间基准、剪辑点、清单字段等）
    3  媒体解码失败或检查点损坏
    4  验收指标超限（码率偏差、对齐误差等）
"""

EXIT_OK = 0
EXIT_INVALID_INPUT = 2
EXIT_MEDIA_CORRUPT = 3
EXIT_METRICS_EXCEEDED = 4


class RelayError(Exception):
    """所有可预期错误的基类，携带进程退出码。"""

    exit_code = EXIT_INVALID_INPUT


class TimebaseError(RelayError):
    """时间基准无法表示或时间换算非法。"""


class TimelineError(RelayError):
    """轨道、剪辑点或转场定义非法（负时长、越界剪辑等）。"""


class PacketRepairError(RelayError):
    """包修复阶段遇到不可恢复的结构问题。"""


class DecodeError(RelayError):
    """离屏解码失败；first_position 记录首个失败位置。"""

    exit_code = EXIT_MEDIA_CORRUPT

    def __init__(self, message: str, first_position: int | None = None):
        super().__init__(message)
        self.first_position = first_position


class CheckpointError(RelayError):
    """检查点索引损坏、输出截断或任务标识冲突。"""

    exit_code = EXIT_MEDIA_CORRUPT

    def __init__(self, message: str, conflict_blocks: list[int] | None = None):
        super().__init__(message)
        self.conflict_blocks = conflict_blocks or []


class ManifestError(RelayError):
    """清单缺失、字段非法或校验不通过。"""


class MetricsExceededError(RelayError):
    """验收指标（码率偏差、对齐误差等）超过阈值。"""

    exit_code = EXIT_METRICS_EXCEEDED
