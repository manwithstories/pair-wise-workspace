"""统一媒体时间基准：清单时间刻度（秒）↔ 输出采样位置，并记录对齐误差。"""
from __future__ import annotations

from dataclasses import dataclass, field


@dataclass(frozen=True)
class AlignmentRecord:
    """单个分片的期望/实际采样位置对照。"""

    sequence: int
    expected_sample: int
    actual_sample: int

    @property
    def error_samples(self) -> int:
        return self.actual_sample - self.expected_sample


@dataclass
class Timeline:
    """把分片媒体时间映射到输出采样位置。"""

    rate: int
    origin: float = 0.0  # 输出采样 0 对应的清单媒体时间（秒）
    records: list[AlignmentRecord] = field(default_factory=list)

    def expected_sample(self, media_time: float) -> int:
        return round((media_time - self.origin) * self.rate)

    def record(self, sequence: int, media_time: float, actual_sample: int) -> None:
        self.records.append(
            AlignmentRecord(sequence, self.expected_sample(media_time), actual_sample)
        )

    def max_error_ms(self) -> float:
        """清单与输出时间的最大对齐误差（毫秒）。"""
        if not self.records:
            return 0.0
        return max(abs(r.error_samples) for r in self.records) * 1000.0 / self.rate
