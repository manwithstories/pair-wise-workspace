"""时间码清单解析：每行一条 SMPTE 记录，支持注释/空行与分块喂入。

清单文本格式（UTF-8）::

    # rate=24            首行可选，缺省取构造参数 default_rate
    00:00:00:00
    00:00:00:01
    ; 分号开头为注释
"""
from __future__ import annotations

from fractions import Fraction

from .model import ErrorRecord, FieldIndexError, MAX_INPUT_BYTES, SourceRef, TimecodeRecord
from .timecode import TimecodeError, parse_rate, parse_timecode


class TimecodeListParser:
    """增量时间码清单解析器，保留每条记录的原始字节偏移。"""

    def __init__(self, source: str, default_rate: Fraction) -> None:
        self.source = source
        self.default_rate = default_rate
        self.records: list[TimecodeRecord] = []
        self.errors: list[ErrorRecord] = []
        self._total = 0
        self._line = bytearray()
        self._line_start = 0
        self._rate = default_rate

    def feed(self, chunk: bytes) -> None:
        if self._total + len(chunk) > MAX_INPUT_BYTES:
            raise FieldIndexError(
                f"输入超过 {MAX_INPUT_BYTES} 字节上限", self._total
            )
        for byte in chunk:
            self._total += 1
            if byte in (0x0A, 0x0D):
                if self._line:
                    self._finish_line()
            elif not self._line:
                self._line_start = self._total - 1
                self._line.append(byte)
            else:
                self._line.append(byte)

    def close(self) -> tuple[list[TimecodeRecord], list[ErrorRecord]]:
        if self._line:
            self._finish_line(truncated=True)
        return self.records, self.errors

    def _finish_line(self, truncated: bool = False) -> None:
        start, end = self._line_start, self._total
        text = bytes(self._line).decode("utf-8", errors="replace").strip()
        self._line.clear()
        if not text or text.startswith(("#", ";")):
            return
        if text.startswith("rate="):
            self._rate = parse_rate(text.split("=", 1)[1])
            return
        try:
            frame, drop = parse_timecode(text, self._rate)
        except TimecodeError as exc:
            self.errors.append(ErrorRecord(
                source=self.source, start=start, end=end,
                stage="timecode", reason=str(exc),
                action="truncated-skip" if truncated else "quarantine",
                recoverable=False,
            ))
            return
        self.records.append(TimecodeRecord(
            frame=frame, rate_fps=self._rate, drop_frame=drop,
            raw=text, ref=SourceRef(self.source, start, end),
        ))


def parse_timecode_bytes(
    data: bytes, source: str, default_rate: Fraction, chunk: int = 4096
) -> tuple[list[TimecodeRecord], list[ErrorRecord]]:
    """一次性解析时间码清单字节串。"""

    parser = TimecodeListParser(source, default_rate)
    for i in range(0, len(data), chunk):
        parser.feed(data[i : i + chunk])
    return parser.close()
