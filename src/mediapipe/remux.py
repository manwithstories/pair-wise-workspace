"""无损重封装：把压缩包按映射写入目标容器，不解码、不重编码。

输出采用 fragmented MP4/MOV（``movflags=+frag_keyframe+empty_moov+default_base_moof``）：
* 每个包落在明确 fragment 边界，中断时已写出部分仍可解析；
* 续跑时重建输出容器，从检查点之后的第一个包继续，负载原样透传；
* 回读验证按轨道比较包数、压缩负载 SHA-256 与时间戳。

注意：fragmented 封装会折叠源文件 edit-list（priming），使前导包
时间戳整体平移到非负轴；压缩字节不变，但该容器级时间线调整会在
``metadata_not_preserved`` 中明确报告，不静默处理。
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field
from typing import Any

import av

from .checkpoint import ensure_safe_path
from .errors import MediaError, Phase, PipelineInternalError
from .mapping import StreamMapping
from .packet import PacketRecord

_FRAG_FLAGS = "+frag_keyframe+empty_moov+default_base_moof"


@dataclass(slots=True)
class RemuxResult:
    output_bytes: int
    packets_written: int
    output_streams: list[dict[str, Any]]
    metadata_not_preserved: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "output_bytes": self.output_bytes,
            "packets_written": self.packets_written,
            "output_streams": self.output_streams,
            "metadata_not_preserved": self.metadata_not_preserved,
        }


def _container_format(path: str) -> str:
    ext = os.path.splitext(path)[1].lower()
    if ext in (".mp4", ".m4a"):
        return "mp4"
    if ext == ".mov":
        return "mov"
    raise PipelineInternalError(
        f"不支持的输出容器扩展名: {ext}（仅 .mp4/.mov）", phase=Phase.REMUX
    )


class Remuxer:
    """逐包 mux；压缩字节、PTS/DTS、time_base 原样透传，不触碰编解码器。"""

    def __init__(
        self,
        output_path: str,
        input_av_streams: list[Any],
        mappings: list[StreamMapping],
        *,
        input_path: str,
    ) -> None:
        self.output_path = ensure_safe_path(output_path, input_path=input_path)
        self.format_name = _container_format(self.output_path)
        self._mappings = mappings
        self._av_by_index = {int(s.index): s for s in input_av_streams}
        self._container: av.container.OutputContainer | None = None
        self._out_streams: dict[int, Any] = {}
        self.packets_written = 0
        self.bytes_written = 0
        self.metadata_not_preserved: list[str] = []

    def open(self) -> "Remuxer":
        try:
            self._container = av.open(
                self.output_path,
                mode="w",
                format=self.format_name,
                options={"movflags": _FRAG_FLAGS},
            )
        except av.FFmpegError as exc:
            raise PipelineInternalError(
                f"输出容器无法打开: {exc}", phase=Phase.REMUX
            ) from exc
        assert self._container is not None
        for m in self._mappings:
            src = self._av_by_index.get(m.source_index)
            if src is None:
                raise PipelineInternalError(
                    f"源流 {m.source_index} 的 AVStream 缺失", phase=Phase.REMUX
                )
            # 以输入流为模板克隆编解码参数；不会触发任何解码/重编码
            self._out_streams[m.source_index] = self._container.add_stream(
                template=src
            )
        # 碎片化封装折叠 edit-list，属于无法逐包保持的容器级元数据
        self.metadata_not_preserved.append("edit_list/priming 时间线平移")
        return self

    def write(self, record: PacketRecord) -> None:
        if self._container is None:
            raise RuntimeError("Remuxer 尚未 open()")
        out_stream = self._out_streams.get(record.stream_index)
        if out_stream is None:
            return  # 未映射流不写出
        if record.dts is None or record.size == 0:
            return  # 末端 flush 空包，不封装
        packet = av.Packet(record.payload.data)
        packet.pts = record.pts
        packet.dts = record.dts
        packet.duration = record.duration
        packet.is_keyframe = record.is_keyframe
        packet.time_base = record.time_base
        packet.stream = out_stream
        try:
            self._container.mux(packet)
        except av.FFmpegError as exc:
            raise PipelineInternalError(
                f"写出失败（源流 {record.stream_index} 包 {record.sequence}）: {exc}",
                phase=Phase.REMUX,
                stream_index=record.stream_index,
            ) from exc
        self.packets_written += 1
        self.bytes_written = os.path.getsize(self.output_path)

    def close(self) -> RemuxResult:
        if self._container is not None:
            try:
                self._container.close()
            except av.FFmpegError as exc:
                raise PipelineInternalError(
                    f"输出容器关闭失败: {exc}", phase=Phase.REMUX
                ) from exc
            self._container = None
        self.bytes_written = (
            os.path.getsize(self.output_path)
            if os.path.exists(self.output_path)
            else 0
        )
        return RemuxResult(
            output_bytes=self.bytes_written,
            packets_written=self.packets_written,
            output_streams=[
                {
                    "output_stream": m.output_index,
                    "source_stream": m.source_index,
                    "codec": self._av_by_index[m.source_index].codec_context.name,
                    "type": self._av_by_index[m.source_index].type,
                }
                for m in self._mappings
            ],
            metadata_not_preserved=sorted(set(self.metadata_not_preserved)),
        )

    def __enter__(self) -> "Remuxer":
        return self.open()

    def __exit__(self, exc_type: object, *exc_rest: object) -> None:
        if self._container is not None:
            try:
                self._container.close()
            except MediaError:
                if exc_type is None:
                    raise
            finally:
                self._container = None
