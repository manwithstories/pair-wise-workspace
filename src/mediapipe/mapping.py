"""轨道映射配置与写出前校验。

映射在 *任何包写出之前* 完成校验：源流缺失、重复映射、类型不符
都会以 :class:`InputValidationError`（退出码 2）失败，并指出源流号。
默认映射：按流编号顺序把全部已知音视频流 1:1 映射到输出。
"""
from __future__ import annotations

from dataclasses import dataclass

from .demux import StreamInfo
from .errors import InputValidationError, Phase


@dataclass(frozen=True, slots=True)
class StreamMapping:
    """一条 输入流 -> 输出流 的映射。"""

    source_index: int
    output_index: int
    expected_type: str

    def to_dict(self) -> dict:
        return {
            "source_stream": self.source_index,
            "output_stream": self.output_index,
            "type": self.expected_type,
        }


def default_mapping(infos: list[StreamInfo]) -> list[StreamMapping]:
    """已知（未跳过）流按编号顺序 1:1 映射。"""
    out: list[StreamMapping] = []
    for out_idx, info in enumerate(i for i in infos if not i.skipped):
        out.append(StreamMapping(info.index, out_idx, info.type))
    return out


def validate_mapping(
    mapping: list[StreamMapping], infos: list[StreamInfo]
) -> list[StreamMapping]:
    """在写出前校验映射，失败即抛 InputValidationError。"""
    if not mapping:
        raise InputValidationError(
            "映射为空：至少需要一条可输出的轨道映射", phase=Phase.MAPPING
        )
    by_index = {info.index: info for info in infos}
    seen_sources: set[int] = set()
    seen_outputs: set[int] = set()
    for m in mapping:
        if m.source_index not in by_index:
            raise InputValidationError(
                f"映射引用了不存在的源流号 {m.source_index}",
                phase=Phase.MAPPING,
                stream_index=m.source_index,
                detail={"known_streams": sorted(by_index)},
            )
        info = by_index[m.source_index]
        if info.skipped:
            raise InputValidationError(
                f"源流 {m.source_index} 类型未知（{info.type}），不可映射",
                phase=Phase.MAPPING,
                stream_index=m.source_index,
            )
        if m.source_index in seen_sources:
            raise InputValidationError(
                f"源流 {m.source_index} 被重复映射",
                phase=Phase.MAPPING,
                stream_index=m.source_index,
            )
        if m.output_index in seen_outputs:
            raise InputValidationError(
                f"输出流号 {m.output_index} 被重复占用（来自源流 {m.source_index}）",
                phase=Phase.MAPPING,
                stream_index=m.source_index,
            )
        if info.type != m.expected_type:
            raise InputValidationError(
                f"源流 {m.source_index} 类型不符：配置要求 {m.expected_type}，"
                f"实际为 {info.type}",
                phase=Phase.MAPPING,
                stream_index=m.source_index,
            )
        seen_sources.add(m.source_index)
        seen_outputs.add(m.output_index)
    expected_outputs = set(range(len(mapping)))
    if seen_outputs != expected_outputs:
        raise InputValidationError(
            f"输出流号必须从 0 连续编号，实际为 {sorted(seen_outputs)}",
            phase=Phase.MAPPING,
        )
    # 输出顺序按 output_index 规范化
    return sorted(mapping, key=lambda m: m.output_index)
