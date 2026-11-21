"""流式解复用：逐包读取 MP4/MOV，不解码压缩负载。

职责：
* 报告每条流的编号、类型、编码、time_base、包数与起止时间；
* 未知流类型可跳过但计入 ``skipped_streams``；
* 检测负 PTS/DTS、DTS 非单调，错误携带阶段与源偏移；
* 容器层损坏（截断、坏 box、坏包、缺失索引）尽量定位首个坏字节，
  无法精确定位时给出半开偏移区间，绝不伪称精确。
"""
from __future__ import annotations

import os
from dataclasses import dataclass, field
from fractions import Fraction
from typing import Any, Iterator

import av

from .errors import (
    InputValidationError,
    MediaCorruptionError,
    Phase,
    SourceOffset,
)
from .packet import PacketRecord, build_record
from .timebase import as_fraction

# PyAV 可稳定重封装的常见类型；其余按“未知流”跳过计数
_KNOWN_TYPES = frozenset({"video", "audio", "subtitle", "data", "attachment"})


@dataclass(slots=True)
class StreamInfo:
    index: int
    type: str
    codec: str
    time_base: Fraction
    start_time_us: int | None = None
    end_time_us: int | None = None
    packet_count: int = 0
    skipped: bool = False
    metadata_keys: list[str] = field(default_factory=list)

    def to_dict(self) -> dict[str, Any]:
        return {
            "stream": self.index,
            "type": self.type,
            "codec": self.codec,
            "time_base": str(self.time_base),
            "start_time_us": self.start_time_us,
            "end_time_us": self.end_time_us,
            "packet_count": self.packet_count,
            "skipped": self.skipped,
            "metadata_keys": self.metadata_keys,
        }


def _classify_av_error(exc: av.FFmpegError, file_size: int) -> MediaCorruptionError:
    """把 PyAV/FFmpeg 异常映射为带偏移的损坏错误。

    MP4 解析在 moov/box 边界失败时通常无法精确到字节，统一给出
    “已确认有效边界 → 文件尾”的半开区间，并在 note 中说明归因依据。
    """
    msg = str(exc)
    errno = getattr(exc, "errno", None)
    truncated_markers = ("End of file", "EOF", "truncat", "Invalid data found")
    looks_truncated = any(m.lower() in msg.lower() for m in truncated_markers)
    kind = "truncated_or_corrupt_box" if looks_truncated else "container_parse_error"
    offset = SourceOffset(
        start=0,
        end=file_size,
        note=f"容器层解析失败，无法精确归因；errno={errno}",
    )
    return MediaCorruptionError(
        f"解复用损坏（{kind}）: {msg}",
        phase=Phase.DEMUX,
        offset=offset,
        detail={"ffmpeg_errno": errno, "corruption_type": kind},
    )


class Demuxer:
    """以上下文管理器方式打开容器并流式产出 :class:`PacketRecord`。"""

    def __init__(self, path: str, *, allow_negative_priming: bool = True) -> None:
        self.path = os.fspath(path)
        if not os.path.isfile(self.path):
            raise InputValidationError(
                f"输入文件不存在或不是普通文件: {self.path}", phase=Phase.DEMUX
            )
        self._file_size = os.path.getsize(self.path)
        if self._file_size == 0:
            raise MediaCorruptionError(
                "输入文件为空（0 字节）",
                phase=Phase.DEMUX,
                offset=SourceOffset(0, 0, "空文件"),
                detail={"corruption_type": "empty_file"},
            )
        self._container: av.container.InputContainer | None = None
        self._streams: dict[int, StreamInfo] = {}
        self.skipped_streams: list[StreamInfo] = []
        self._last_dts_us: dict[int, int] = {}
        # MP4 编码器延迟/edit list 会让前导包 PTS/DTS 为负（priming），属正常；
        # 按 (流, 字段) 分别跟踪，进入非负区后再出现负值即为真损坏。
        self._allow_negative_priming = allow_negative_priming
        self._seen_non_negative: dict[tuple[int, str], bool] = {}
        self.negative_priming: dict[int, int] = {}
        self._sequence = 0

    # ---- 打开与流信息 -------------------------------------------------
    def open(self) -> "Demuxer":
        try:
            self._container = av.open(self.path, mode="r", metadata_errors="warn")
        except av.FFmpegError as exc:
            raise _classify_av_error(exc, self._file_size) from exc
        except av.AVError as exc:  # 打开期其它底层错误
            raise MediaCorruptionError(
                f"容器无法打开: {exc}",
                phase=Phase.DEMUX,
                offset=SourceOffset(0, self._file_size, "open 失败，无法归因"),
            ) from exc
        for stream in self._container.streams:
            stype = getattr(stream, "type", "unknown") or "unknown"
            tb = as_fraction(
                stream.time_base or Fraction(1, 90000)
            )
            info = StreamInfo(
                index=int(stream.index),
                type=stype,
                codec=getattr(stream.codec_context, "name", "unknown") or "unknown",
                time_base=tb,
                metadata_keys=sorted((stream.metadata or {}).keys()),
            )
            if stype not in _KNOWN_TYPES:
                info.skipped = True
                self.skipped_streams.append(info)
            self._streams[info.index] = info
        return self

    @property
    def file_size(self) -> int:
        return self._file_size

    @property
    def stream_infos(self) -> list[StreamInfo]:
        return list(self._streams.values())

    @property
    def av_streams(self) -> list[Any]:
        """底层 PyAV 流（重封装时作为输出流模板）。"""
        if self._container is None:
            raise RuntimeError("Demuxer 尚未 open()")
        return list(self._container.streams)

    def info(self, stream_index: int) -> StreamInfo:
        return self._streams[stream_index]

    # ---- 逐包迭代 -----------------------------------------------------
    def iter_packets(self) -> Iterator[PacketRecord]:
        if self._container is None:
            raise RuntimeError("Demuxer 尚未 open()")
        container = self._container
        try:
            for packet in container.demux():
                try:
                    yield from self._handle_packet(packet)
                except MediaCorruptionError:
                    raise
        except av.FFmpegError as exc:
            raise _classify_av_error(exc, self._file_size) from exc

    def _handle_packet(self, packet: Any) -> Iterator[PacketRecord]:
        sidx = int(packet.stream_index)
        info = self._streams.get(sidx)
        if info is not None and info.skipped:
            return  # 未知类型：跳过但流已计数
        if packet.dts is None:
            # 解复用末端的 flush 空包，无媒体时间，不进入流水线
            return
        pos = getattr(packet, "pos", None)
        if pos is None or pos < 0:
            # 无字节定位（如流式 muxer 产物）：给出序号级区间，明确不可精确归因
            src_off, src_end = self._sequence, self._sequence + 1
            pos_note = "无文件字节位置（packet.pos 缺失）"
        else:
            src_off, src_end = int(pos), int(pos) + packet.size
            pos_note = ""
        # PyAV 的损坏标记（含 ffprobe 语义上的 corrupt packet）
        if bool(getattr(packet, "is_corrupt", False)):
            raise MediaCorruptionError(
                f"流 {sidx} 第 {self._sequence} 包被解码器标记为损坏",
                phase=Phase.DEMUX,
                stream_index=sidx,
                offset=SourceOffset(
                    src_off,
                    src_end,
                    pos_note or "is_corrupt 标记",
                ),
                detail={"corruption_type": "corrupt_packet_flag"},
            )
        record = build_record(
            packet,
            self._sequence,
            source_offset=src_off,
            source_end=src_end,
        )
        self._check_timestamps(record)
        self._sequence += 1
        info = self._streams[record.stream_index]
        info.packet_count += 1
        if record.pts_us is not None:
            info.start_time_us = (
                record.pts_us
                if info.start_time_us is None
                else min(info.start_time_us, record.pts_us)
            )
            info.end_time_us = (
                record.pts_us
                if info.end_time_us is None
                else max(info.end_time_us, record.pts_us)
            )
        yield record

    def _check_timestamps(self, record: PacketRecord) -> None:
        sidx = record.stream_index
        for name, value in (("pts", record.pts), ("dts", record.dts)):
            if value is None:
                continue
            key = (sidx, name)
            if value < 0 and not self._seen_non_negative.get(key, False):
                if self._allow_negative_priming:
                    # edit-list / 编码器 priming：前导若干包可为负，记录但不判损坏
                    self.negative_priming[sidx] = (
                        self.negative_priming.get(sidx, 0) + 1
                    )
                    continue
                raise MediaCorruptionError(
                    f"流 {sidx} 包 {record.sequence} 出现负 {name.upper()}: {value}",
                    phase=Phase.DEMUX,
                    stream_index=sidx,
                    offset=SourceOffset(
                        record.payload.source_offset,
                        record.payload.source_end,
                        f"负 {name.upper()}",
                    ),
                    detail={"corruption_type": f"negative_{name}"},
                )
            if value < 0:
                raise MediaCorruptionError(
                    f"流 {sidx} 包 {record.sequence} 出现非法负 {name.upper()}: {value}"
                    "（该字段时间轴已进入非负区后再次为负）",
                    phase=Phase.DEMUX,
                    stream_index=sidx,
                    offset=SourceOffset(
                        record.payload.source_offset,
                        record.payload.source_end,
                        f"非法负 {name.upper()}",
                    ),
                    detail={"corruption_type": f"negative_{name}"},
                )
            self._seen_non_negative[key] = True
        if record.dts_us is not None:
            prev = self._last_dts_us.get(record.stream_index)
            if prev is not None and record.dts_us < prev:
                raise MediaCorruptionError(
                    f"流 {record.stream_index} 包 {record.sequence} DTS 非单调: "
                    f"{record.dts_us}us < 前包 {prev}us",
                    phase=Phase.DEMUX,
                    stream_index=record.stream_index,
                    offset=SourceOffset(
                        record.payload.source_offset,
                        record.payload.source_end,
                        "DTS 回退",
                    ),
                    detail={"corruption_type": "dts_non_monotonic"},
                )
            self._last_dts_us[record.stream_index] = record.dts_us

    def close(self) -> None:
        if self._container is not None:
            self._container.close()
            self._container = None

    def __enter__(self) -> "Demuxer":
        return self.open()

    def __exit__(self, *exc: object) -> None:
        self.close()
