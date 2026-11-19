"""H.264 / H.265 / AV1 裸码流分块扫描器。

设计约束：
- 超长流按块读取（默认 1 MiB），常驻内存只与块大小相关；
- 每个编码单元记录输入文件中的原始字节偏移；
- 截断/损坏单元标记隔离后继续扫描后续可恢复边界，不抛出中断；
- 不做完整解码，仅解析 NAL 头 / OBU 头用于类型与关键帧判定。
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field

from .timeline import ErrorKind, MediaError, Stage

CODECS = ("h264", "h265", "av1")
H264_IDR, H264_SPS = 5, 7
H265_IDR_TYPES = frozenset({19, 20})
H265_CRA, H265_SPS = 21, 33
OBU_SEQ_HEADER, OBU_FRAME, OBU_FRAME_HEADER = 1, 6, 7


@dataclass(slots=True)
class CodecUnit:
    codec: str
    unit_type: int
    start_offset: int       # 起始码/OBU 头首字节在输入中的偏移
    payload_offset: int     # 有效载荷首字节偏移
    end_offset: int         # 独占式结束偏移
    keyframe: bool = False
    truncated: bool = False
    corrupt: bool = False
    notes: list[str] = field(default_factory=list)

    @property
    def size(self) -> int:
        return self.end_offset - self.start_offset

    def to_dict(self) -> dict:
        return {
            "codec": self.codec, "unit_type": self.unit_type,
            "start_offset": self.start_offset,
            "payload_offset": self.payload_offset,
            "end_offset": self.end_offset, "size": self.size,
            "keyframe": self.keyframe, "truncated": self.truncated,
            "corrupt": self.corrupt, "notes": self.notes,
        }


@dataclass(slots=True)
class ScanResult:
    codec: str
    units: list[CodecUnit]
    errors: list[MediaError]
    total_bytes: int
    truncated: bool

    def keyframe_offsets(self) -> list[int]:
        return [u.start_offset for u in self.units if u.keyframe]

    def healthy_units(self) -> list[CodecUnit]:
        return [u for u in self.units if not u.truncated and not u.corrupt]


def _annexb_type(codec: str, first_byte: int) -> tuple[int, bool, bool]:
    """返回 (unit_type, keyframe, corrupt)。"""

    corrupt = bool(first_byte & 0x80)  # forbidden_zero_bit
    if codec == "h264":
        unit_type = first_byte & 0x1F
        return unit_type, unit_type == H264_IDR, corrupt
    unit_type = (first_byte >> 1) & 0x3F
    keyframe = unit_type in H265_IDR_TYPES or unit_type == H265_CRA
    return unit_type, keyframe, corrupt


class AnnexBScanner:
    """Annex B（00 00 01 / 00 00 00 01）流式状态机，逐块喂入即可。"""

    def __init__(self, codec: str) -> None:
        self.codec = codec
        self.units: list[CodecUnit] = []
        self.errors: list[MediaError] = []
        self._zeros = 0
        self._cur: CodecUnit | None = None
        self._pos = -1

    def feed(self, chunk: bytes) -> None:
        base = self._pos + 1
        for rel, byte in enumerate(chunk):
            self._pos = base + rel
            cur = self._cur
            if cur is not None and cur.unit_type == -1:
                # 起始码 01 之后的首字节即 NAL 头
                cur.unit_type, cur.keyframe, cur.corrupt = _annexb_type(
                    self.codec, byte)
                if cur.corrupt:
                    cur.notes.append("forbidden_zero_bit 置位")
                    self.errors.append(MediaError(
                        Stage.PARSE, ErrorKind.CORRUPT_UNIT, cur.start_offset,
                        f"{self.codec} NAL 禁止位被置位", recovered=True,
                        context={"unit_type": cur.unit_type}))
                continue
            if byte == 0:
                self._zeros = min(self._zeros + 1, 4)
                continue
            if byte == 1 and self._zeros >= 2:
                self._close(self._pos - self._zeros - 1)
                payload_pos = self._pos + 1
                self._cur = CodecUnit(
                    self.codec, -1, payload_pos - self._zeros - 1,
                    payload_pos, -1)
            self._zeros = 0

    def _close(self, end_offset: int) -> None:
        cur = self._cur
        if cur is None:
            return
        cur.end_offset = end_offset
        if cur.payload_offset >= end_offset:
            cur.corrupt = True
            cur.notes.append("空单元")
            self.errors.append(MediaError(
                Stage.PARSE, ErrorKind.CORRUPT_UNIT, cur.start_offset,
                f"{self.codec} 单元无有效载荷", recovered=True))
        self.units.append(cur)
        self._cur = None

    def finish(self, total: int, stream_truncated: bool) -> ScanResult:
        if self._cur is not None:
            cur = self._cur
            cur.end_offset = total
            if cur.unit_type == -1:  # 起始码恰好位于文件末尾
                cur.truncated = True
            self.units.append(cur)
            self._cur = None
        if stream_truncated and self.units:
            last = self.units[-1]
            last.truncated = True
            last.notes.append("码流在单元中途被截断")
            self.errors.append(MediaError(
                Stage.PARSE, ErrorKind.TRUNCATED_UNIT, last.start_offset,
                f"{self.codec} 末单元在字节 {total} 处截断", recovered=False))
        return ScanResult(self.codec, self.units, self.errors, total,
                          stream_truncated)


class AV1Scanner:
    """长度前缀 AV1 低开销流（IVF 之外的裸 OBU 序列）流式状态机。"""

    def __init__(self) -> None:
        self.units: list[CodecUnit] = []
        self.errors: list[MediaError] = []
        self._saw_seq = False
        self._stop = False
        self._state = "header"
        self._unit: CodecUnit | None = None
        self._leb = 0
        self._leb_bytes = 0
        self._remain = 0
        self._first_payload = -1
        self._pos = -1

    def feed(self, chunk: bytes) -> None:
        base = self._pos + 1
        for rel, byte in enumerate(chunk):
            self._pos = base + rel
            if self._stop:
                return
            self._step(byte)

    def _step(self, byte: int) -> None:
        pos = self._pos
        if self._state == "header":
            if byte & 0x80:
                self._emit_bad(pos, "AV1 OBU forbidden 位被置位", corrupt=True)
                return
            obu_type = (byte >> 3) & 0x0F
            has_size = bool(byte & 0x02)
            payload_start = pos + 2 + (1 if byte & 0x04 else 0)
            self._unit = CodecUnit("av1", obu_type, pos, payload_start, -1)
            if not has_size:
                self._emit_bad(pos, "裸流要求 OBU 携带长度字段", corrupt=True)
                return
            self._leb, self._leb_bytes, self._state = 0, 0, "leb"
        elif self._state == "leb":
            self._leb |= (byte & 0x7F) << (7 * self._leb_bytes)
            self._leb_bytes += 1
            if self._leb_bytes > 8:
                self._emit_bad(pos, "AV1 LEB128 超过 8 字节", truncated=True)
                return
            if not (byte & 0x80):
                self._remain = self._leb
                self._first_payload = -1
                self._state = "payload"
        else:  # payload
            if self._first_payload == -1:
                self._first_payload = byte
            self._remain -= 1
            if self._remain == 0:
                self._finish_unit(pos + 1)

    def _finish_unit(self, end: int) -> None:
        unit = self._unit
        assert unit is not None
        unit.end_offset = end
        if unit.unit_type == OBU_SEQ_HEADER:
            self._saw_seq = True
        elif unit.unit_type in (OBU_FRAME, OBU_FRAME_HEADER):
            show_existing = (self._first_payload >> 7) & 1
            frame_type = ((self._first_payload >> 4) & 7
                          if not show_existing else -1)
            unit.keyframe = self._saw_seq and frame_type == 0
        self.units.append(unit)
        self._unit, self._state = None, "header"

    def _emit_bad(self, pos: int, message: str, *, corrupt: bool = False,
                  truncated: bool = False) -> None:
        unit = self._unit
        kind = ErrorKind.CORRUPT_UNIT if corrupt else ErrorKind.TRUNCATED_UNIT
        if unit is not None:
            unit.end_offset = pos + 1
            unit.corrupt, unit.truncated = corrupt, truncated
            self.units.append(unit)
            self._unit = None
        self.errors.append(MediaError(
            Stage.PARSE, kind, pos, message, recovered=False))
        self._stop = True

    def finish(self, total: int, stream_truncated: bool) -> ScanResult:
        if self._unit is not None:  # EOF 落在 leb/payload 中途
            self._unit.end_offset = total
            self._unit.truncated = True
            self._unit.notes.append("OBU 在文件末尾截断")
            self.units.append(self._unit)
            self._unit = None
            self.errors.append(MediaError(
                Stage.PARSE, ErrorKind.TRUNCATED_UNIT,
                self.units[-1].start_offset,
                f"AV1 OBU 截断，已接收 {total} 字节", recovered=False))
        elif stream_truncated and self.units:
            self.units[-1].truncated = True
            self.errors.append(MediaError(
                Stage.PARSE, ErrorKind.TRUNCATED_UNIT,
                self.units[-1].start_offset, "AV1 码流在边界处截断",
                recovered=False))
        return ScanResult("av1", self.units, self.errors, total,
                          stream_truncated)


def scan_bytes(data: bytes, codec: str,
               stream_truncated: bool = False) -> ScanResult:
    if codec not in CODECS:
        raise ValueError(f"不支持的编码标识: {codec!r}，支持 {CODECS}")
    if codec == "av1":
        scanner: AnnexBScanner | AV1Scanner = AV1Scanner()
        scanner.feed(data)
        return scanner.finish(len(data), stream_truncated)
    scanner = AnnexBScanner(codec)
    scanner.feed(data)
    return scanner.finish(len(data), stream_truncated)


def scan_file(path: str, codec: str, *, chunk_size: int = 1 << 20,
              stream_truncated: bool = False) -> ScanResult:
    """分块扫描，起始码/LEB 跨块时状态机自动续接。"""

    if codec not in CODECS:
        raise ValueError(f"不支持的编码标识: {codec!r}，支持 {CODECS}")
    total = os.path.getsize(path)
    scanner: AnnexBScanner | AV1Scanner = (
        AV1Scanner() if codec == "av1" else AnnexBScanner(codec))
    with open(path, "rb") as fh:
        while True:
            chunk = fh.read(chunk_size)
            if not chunk:
                break
            scanner.feed(chunk)
    return scanner.finish(total, stream_truncated)
