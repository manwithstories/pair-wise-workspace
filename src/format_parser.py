"""自研交换格式解析器（唯一语法实现）。

决策表与素材清单复用本模块，禁止另立第二套语法。

块语法（缩进仅为可读性，解析时忽略）::

    EXCHANGE version=1 timebase=24/1
      ASSET A01 path=... size=... fingerprint=...
        ALIAS name=cam01 target=A01
      END
      SEQUENCE S1
        TRACK V1
          CLIP c1 asset=A01 dst_in=0 dst_out=12 src_in=5 src_out=17
            SPEED dst_in=0 dst_out=8 rate=3/2
          END
        END
      END
    END

规则：
- 未知 ``KEY=VALUE`` 字段不报错，原样保留在 ``Block.fields`` 中交由语义层；
- 未知块、块嵌套错误、字段类型错误等结构问题抛 :class:`FormatError`，携带行号；
- 顶层块在遇到其 ``END`` 时即产出，支持大清单流式读取。
"""

from __future__ import annotations

from dataclasses import dataclass, field
from fractions import Fraction
from pathlib import Path
from typing import Dict, Iterator, List, Optional, Sequence, Tuple

from .errors import FormatError
from .model import TimeBase

_KNOWN_BLOCKS = {"EXCHANGE", "ASSET", "SEQUENCE", "TRACK", "CLIP", "ALIAS", "SPEED"}
# 可作叶式书写（不跟 END）的块；其余块必须显式 END
_LEAF_BLOCKS = {"ALIAS", "SPEED"}
# 块 -> 允许的父块（EXCHANGE 只能做根）
_PARENT_RULES = {
    "EXCHANGE": (None,),
    "ASSET": ("EXCHANGE",),
    "SEQUENCE": ("EXCHANGE",),
    "TRACK": ("SEQUENCE",),
    "CLIP": ("TRACK",),
    "ALIAS": ("ASSET",),
    "SPEED": ("CLIP",),
}


@dataclass(frozen=True)
class Block:
    kind: str
    ident: str
    fields: Dict[str, str] = field(default_factory=dict)
    children: Tuple["Block", ...] = ()
    line_start: int = 0
    line_end: int = 0


# ---------- 标量解析 ----------

def parse_int(text: str, line: int, field_name: str, source: str = "") -> int:
    try:
        return int(text.strip())
    except (ValueError, AttributeError):
        raise FormatError(f"字段 {field_name} 需要整数，得到 {text!r}", line, source)


def parse_fraction(text: str, line: int = 0, field_name: str = "rate",
                   source: str = "") -> Fraction:
    """支持整数、小数与分数（含负值，负速率表示反向）。"""
    text = text.strip()
    try:
        if "/" in text:
            num, den = text.split("/", 1)
            return Fraction(int(num), int(den))
        if "." in text:
            return Fraction(text)
        return Fraction(int(text), 1)
    except (ValueError, ZeroDivisionError):
        raise FormatError(f"字段 {field_name} 需要分数/小数，得到 {text!r}", line, source)


def parse_timecode(text: str, timebase: TimeBase, line: int = 0,
                   source: str = "") -> int:
    """``HH:MM:SS:FF`` 时间码或裸帧整数 -> 帧序号。"""
    text = text.strip()
    if ":" not in text:
        return parse_int(text, line, "时间码", source)
    parts = text.split(":")
    if len(parts) != 4:
        raise FormatError(f"时间码需要 HH:MM:SS:FF 或裸帧，得到 {text!r}", line, source)
    hh, mm, ss, ff = (parse_int(p, line, "时间码分量", source) for p in parts)
    if timebase.rate.denominator != 1:
        raise FormatError("非整数时间基下请使用裸帧整数表达时间码", line, source)
    fps = timebase.rate.numerator
    if not (0 <= ff < fps):
        raise FormatError(f"帧分量 {ff} 超出时间基 {timebase}", line, source)
    return ((hh * 60 + mm) * 60 + ss) * fps + ff


# ---------- 行切分 ----------

def _split_head(tokens: Sequence[str], line: int, source: str) -> Tuple[str, str, Dict[str, str]]:
    kind = tokens[0]
    if kind not in _KNOWN_BLOCKS:
        raise FormatError(f"未知块类型 {kind!r}", line, source)
    ident = ""
    fields: Dict[str, str] = {}
    for tok in tokens[1:]:
        if "=" not in tok:
            if ident:
                raise FormatError(f"块 {kind} 出现多余的裸标识符 {tok!r}", line, source)
            ident = tok
            continue
        key, value = tok.split("=", 1)
        fields[key] = value
    return kind, ident, fields


def _iter_lines(lines: Iterator[str]) -> Iterator[Tuple[int, str]]:
    for idx, raw in enumerate(lines, start=1):
        text = raw.strip()
        if text and not text.startswith("#"):
            yield idx, text


# ---------- 块流解析 ----------

def iter_blocks(lines: Iterator[str], source: str = "") -> Iterator[Block]:
    """流式产出顶层块；嵌套块挂在父块 ``children`` 中。

    块的 END 允许省略：当一个块开始后，遇到另一个同级开始块或父级 END 时，
    前一块自动关闭。显式 ``END [KIND]`` 优先做配对校验。ALIAS/SPEED 这类
    纯叶块同样支持显式/隐式两种结束方式。
    """
    stack: List[List[object]] = []   # 每项 [kind, ident, fields, children, start-line]
    roots: List[Block] = []

    def build(frame: List[object]) -> Block:
        kind, ident, fields, children, start = frame  # type: ignore[misc]
        return Block(kind, ident, dict(fields), tuple(children), start, current_line)  # type: ignore[arg-type]

    current_line = 0
    for current_line, text in _iter_lines(lines):
        tokens = text.split()
        head = tokens[0]
        if head == "END":
            target = tokens[1] if len(tokens) > 1 else None
            if not stack:
                raise FormatError("END 没有匹配的开始块", current_line, source)
            if target is not None and target != stack[-1][0]:
                # 显式 END 名字不符：可能是叶块的 END，向上找匹配帧
                match = next((f for f in reversed(stack) if f[0] == target), None)
                if match is None:
                    raise FormatError(
                        f"END {target} 与开始块 {stack[-1][0]} 不匹配",
                        current_line, source)
                while stack[-1] is not match:
                    block = build(stack.pop())
                    stack[-1][3].append(block)  # type: ignore[index]
            block = build(stack.pop())
            if stack:
                stack[-1][3].append(block)  # type: ignore[index]
            else:
                roots.append(block)
                yield block
            continue

        kind, ident, fields = _split_head(tokens, current_line, source)
        parent = stack[-1][0] if stack else None
        if parent not in _PARENT_RULES[kind]:
            raise FormatError(f"块 {kind} 不能嵌套在 {parent or '根'} 之下",
                              current_line, source)
        frame = [kind, ident, fields, [], current_line]
        if kind in _LEAF_BLOCKS:
            # 纯叶块：直接挂到父块，不压栈；其后的同名 END 由上面的回溯逻辑吸收
            if not stack:
                raise FormatError(f"块 {kind} 不能出现在根层级", current_line, source)
            stack[-1][3].append(build(frame))  # type: ignore[index]
            continue
        stack.append(frame)

    # 文件结束：自动关闭所有未显式 END 的块（叶式语法）
    while len(stack) > 1:
        block = build(stack.pop())
        stack[-1][3].append(block)  # type: ignore[index]
    if stack:
        block = build(stack.pop())
        roots.append(block)
        yield block
    if not roots or roots[0].kind != "EXCHANGE" or len(roots) > 1:
        raise FormatError("文件必须以唯一的 EXCHANGE ... END 作为根块", 0, source)


def parse_blocks(lines: Iterator[str], source: str = "") -> Block:
    root: Optional[Block] = None
    for block in iter_blocks(lines, source):
        root = block
    assert root is not None
    return root


def parse_file(path: Path) -> Block:
    """按文本文件解析（逐行读取，不一次性载入）。"""
    source = str(path)
    with path.open("r", encoding="utf-8") as fh:
        return parse_blocks(iter(fh), source)


def parse_text(text: str, source: str = "") -> Block:
    return parse_blocks(iter(text.splitlines()), source)


# ---------- 字段取值小工具（语义层共用） ----------

def require_field(block: Block, key: str, source: str = "") -> str:
    if key not in block.fields:
        raise FormatError(f"{block.kind} {block.ident} 缺少必填字段 {key}",
                          block.line_start, source)
    return block.fields[key]


def child_blocks(block: Block, kind: str) -> Tuple[Block, ...]:
    return tuple(c for c in block.children if c.kind == kind)
