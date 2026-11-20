"""SRT/WebVTT 增量解析器：逐块喂入字节，保留每条记录的原始字节偏移。

设计要点：
- 字节级状态机，天然支持 UTF-8 BOM、CRLF/CR/LF 混用与换行跨块；
- 尾部截断 cue 记为错误并隔离，后续完整 cue 仍可正常解析；
- 支持 WebVTT cue 标识行、NOTE/REGION/STYLE 块与毫秒级时间戳；
- 单条文本与输入总量受 model 中上限约束，超限抛出带偏移的错误。
"""
from __future__ import annotations

from fractions import Fraction

from .model import (
    Cue,
    ErrorRecord,
    FieldIndexError,
    MAX_CUE_CHARS,
    MAX_INPUT_BYTES,
    SourceRef,
)

_MS = Fraction(1, 1000)
_BOM = b"\xef\xbb\xbf"


class SubtitleParseError(FieldIndexError):
    """字幕流语法错误（携带字节偏移）。"""


def parse_timestamp(token: str) -> Fraction:
    """SRT/VTT 时间戳 -> 秒（Fraction，毫秒精度；SRT 逗号或 VTT 点号分隔）。"""

    token = token.strip().replace(",", ".")
    parts = token.split(":")
    if len(parts) == 3:
        hh, mm, rest = parts
    elif len(parts) == 2:
        hh, mm, rest = "0", parts[0], parts[1]
    else:
        raise SubtitleParseError(f"无法识别的时间戳: {token!r}")
    if "." not in rest:
        raise SubtitleParseError(f"无法识别的时间戳: {token!r}")
    ss, ms = rest.split(".", 1)
    if not (hh.isdigit() and mm.isdigit() and ss.isdigit() and ms.isdigit()):
        raise SubtitleParseError(f"无法识别的时间戳: {token!r}")
    return (int(hh) * 3600 + int(mm) * 60 + int(ss)) + int(ms) * _MS


def split_timing(line: str) -> tuple[Fraction, Fraction] | None:
    """识别时间轴行，返回 (start, end)；非时间轴行返回 None。"""

    if " --> " not in line:
        return None
    left, right = line.split(" --> ", 1)
    return parse_timestamp(left), parse_timestamp(right.split(" ", 1)[0].strip())


class SubtitleParser:
    """增量字幕解析器；通过 feed/close 逐块消费字节流。

    字节偏移始终相对原始流（含 BOM），每条产出都可回溯到原始字节区间。
    """

    def __init__(self, source: str, fmt: str = "auto") -> None:
        self.source = source
        self.fmt = fmt
        self.cues: list[Cue] = []
        self.errors: list[ErrorRecord] = []
        self._total = 0                 # 已消费原始字节数（含 BOM）
        self._line = bytearray()        # 当前未结束行
        self._line_start = 0
        self._block: list[str] = []
        self._block_start = 0
        self._have_block = False
        self._skip_block = False        # WEBVTT/NOTE/REGION/STYLE 块
        self._note_blank = False
        self._head = bytearray()        # 起始 3 字节 BOM 探测
        self._bom_checked = False
        self._bom_size = 0
        self._pending_cr = False

    def feed(self, chunk: bytes) -> None:
        if self._total + len(chunk) > MAX_INPUT_BYTES:
            raise FieldIndexError(
                f"输入超过 {MAX_INPUT_BYTES} 字节上限", self._total
            )
        pos = 0
        if not self._bom_checked:
            take = min(3 - len(self._head), len(chunk))
            self._head.extend(chunk[:take])
            pos = take
            if len(self._head) == 3:
                self._bom_checked = True
                head = bytes(self._head)
                self._head.clear()
                if head == _BOM:
                    self._bom_size = 3
                else:
                    # 无 BOM：头部字节属于真实内容，按原始偏移补消费
                    for offset, byte in enumerate(head, start=1):
                        self._total = offset
                        self._consume(byte)
        for byte in chunk[pos:]:
            self._total += 1
            if self._total <= self._bom_size:
                continue
            self._consume(byte)

    def close(self) -> tuple[list[Cue], list[ErrorRecord]]:
        if self._head:  # 流整体不足 3 字节，无 BOM，按普通内容处理
            for byte in bytes(self._head):
                self._total += 1
                self._consume(byte)
            self._head.clear()
        if self._line:
            self._finish_line()
        if self._have_block:
            self._emit_block(truncated=True)
        return self.cues, self.errors

    # ---- 状态机 ------------------------------------------------------

    def _consume(self, byte: int) -> None:
        if self._pending_cr:
            self._pending_cr = False
            if byte == 0x0A:
                return
        if byte in (0x0A, 0x0D):
            if byte == 0x0D:
                self._pending_cr = True
            self._finish_line()
        else:
            if not self._line:
                self._line_start = self._total - 1
            self._line.append(byte)

    def _finish_line(self) -> None:
        text = bytes(self._line).decode("utf-8", errors="replace").strip()
        self._line.clear()
        if text == "":
            if self._have_block and self._skip_block:
                self._note_blank = True
                return
            if self._have_block:
                self._emit_block()
            return
        if self._skip_block and self._note_blank:
            self._block = []
            self._have_block = False
            self._skip_block = False
            self._note_blank = False
        if not self._have_block:
            self._have_block = True
            self._block_start = self._line_start
            self._skip_block = text.startswith(
                ("WEBVTT", "NOTE", "REGION", "STYLE")
            )
            self._note_blank = False
        self._block.append(text)

    def _emit_block(self, truncated: bool = False) -> None:
        lines, start, end = self._block, self._block_start, self._total
        skip = self._skip_block
        self._block = []
        self._have_block = False
        self._skip_block = False
        self._note_blank = False
        if skip:
            return
        timing_idx = next((i for i, ln in enumerate(lines) if " --> " in ln), -1)
        if timing_idx < 0:
            reason = ("尾部截断 cue：未出现完整时间轴" if truncated
                      else "缺少时间轴行")
            self._quarantine(start, end, reason)
            return
        try:
            timing = split_timing(lines[timing_idx])
        except SubtitleParseError as exc:
            self._quarantine(start, end, f"时间戳非法: {exc}")
            return
        assert timing is not None
        start_t, end_t = timing
        payload = [t for t in lines[timing_idx + 1:] if t.strip()]
        if truncated and not payload:
            self._quarantine(start, end, "尾部截断 cue：缺少文本行")
            return
        text = "\n".join(payload)
        if len(text) > MAX_CUE_CHARS:
            self._quarantine(start, end, f"字幕文本超过 {MAX_CUE_CHARS} 字符上限")
            return
        if end_t <= start_t:
            self._quarantine(start, end, "cue 结束时间不晚于开始时间")
            return
        self.cues.append(
            Cue(
                seq=len(self.cues) + 1,
                start=start_t,
                end=end_t,
                text=text,
                ref=SourceRef(self.source, start, end),
            )
        )

    def _quarantine(self, start: int, end: int, reason: str) -> None:
        self.errors.append(
            ErrorRecord(
                source=self.source,
                start=start,
                end=end,
                stage="subtitles",
                reason=reason,
                action="truncated-skip" if "截断" in reason else "quarantine",
                recoverable=False,
            )
        )


def parse_subtitle_bytes(
    data: bytes, source: str, fmt: str = "auto", chunk: int = 37
) -> tuple[list[Cue], list[ErrorRecord]]:
    """一次性解析字节串（内部按增量路径分块喂入，默认块刻意取小）。"""

    parser = SubtitleParser(source, fmt)
    for i in range(0, len(data), chunk):
        parser.feed(data[i : i + chunk])
    return parser.close()
