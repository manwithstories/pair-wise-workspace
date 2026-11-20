"""色彩元数据检查与裁决。

检查四个字段：range（色彩范围）、matrix（矩阵）、transfer（传递函数）、
primaries（原色）。缺失值按显式策略补齐；多来源冲突时保留各来源原值，
写入 COLOR_CONFLICT 错误摘要，并按确定性优先级裁决出生效值。
"""

from __future__ import annotations

import dataclasses
from typing import Any

from .errors import ErrorBook, ErrorCode

FIELDS = ("range", "matrix", "transfer", "primaries")

# ITU-T H.273 常用枚举 -> 名称；未知整数保留为 "unknown(<n>)"。
RANGE_TERMS = {"limited", "full"}
MATRIX_TERMS = {"bt709", "bt601", "smpte2085", "bt2020nc", "unspecified"}
TRANSFER_TERMS = {"bt709", "smpte2084", "hlg", "iec61966-2-1", "unspecified"}
PRIMARY_TERMS = {"bt709", "bt2020", "smpte432", "unspecified"}

# 冲突裁决优先级：第一个存在的值胜出，顺序固定保证确定性。
CONFLICT_PRIORITY = {
    "range": ("limited", "full"),
    "matrix": ("bt709", "bt2020nc", "smpte2085", "bt601", "unspecified"),
    "transfer": ("bt709", "smpte2084", "hlg", "iec61966-2-1", "unspecified"),
    "primaries": ("bt709", "bt2020", "smpte432", "unspecified"),
}


@dataclasses.dataclass(frozen=True)
class ColorMetadata:
    range: str | None = None
    matrix: str | None = None
    transfer: str | None = None
    primaries: str | None = None

    def to_dict(self) -> dict[str, str | None]:
        return {f: getattr(self, f) for f in FIELDS}

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> "ColorMetadata":
        return cls(**{f: data.get(f) for f in FIELDS})

    def missing(self) -> list[str]:
        return [f for f in FIELDS if getattr(self, f) is None]


# 显式补齐策略：未声明时的缺省值表，调用方必须传入（不允许隐式猜测）。
DEFAULT_FILL_POLICY: dict[str, str] = {
    "range": "limited",
    "matrix": "bt709",
    "transfer": "bt709",
    "primaries": "bt709",
}

_TERM_TABLE = {
    "range": RANGE_TERMS,
    "matrix": MATRIX_TERMS,
    "transfer": TRANSFER_TERMS,
    "primaries": PRIMARY_TERMS,
}


def _validate_terms(meta: ColorMetadata, source: str, errors: ErrorBook) -> None:
    for field in FIELDS:
        value = getattr(meta, field)
        if value is None:
            continue
        if value not in _TERM_TABLE[field]:
            errors.add(
                ErrorCode.COLOR_CONFLICT,
                f"{source}: 色彩字段 {field}={value!r} 不在已知术语表内",
                source=source,
                fatal=False,
                detail={"field": field, "value": value},
            )


def fill_missing(
    meta: ColorMetadata,
    policy: dict[str, str],
    *,
    source: str,
    errors: ErrorBook,
) -> ColorMetadata:
    """按显式策略补齐缺失字段，逐项记录 COLOR_MISSING。"""
    values = meta.to_dict()
    for field in meta.missing():
        if field not in policy:
            errors.add(
                ErrorCode.COLOR_CONFLICT,
                f"{source}: 字段 {field} 缺失且补齐策略未声明，拒绝隐式猜测",
                source=source,
                fatal=False,
                detail={"field": field},
            )
            continue
        errors.add(
            ErrorCode.COLOR_MISSING,
            f"{source}: 色彩字段 {field} 缺失，按策略补为 {policy[field]!r}",
            source=source,
            fatal=False,
            detail={"field": field, "filled": policy[field]},
        )
        values[field] = policy[field]
    return ColorMetadata(**values)


def reconcile(
    observations: dict[str, ColorMetadata],
    *,
    fill_policy: dict[str, str] | None = None,
    errors: ErrorBook | None = None,
) -> ColorMetadata:
    """裁决多来源色彩元数据。

    ``observations`` 为 {来源名: 元数据}。冲突保留全部来源值并写入错误摘要，
    生效值按 :data:`CONFLICT_PRIORITY` 确定性选择；缺失按 ``fill_policy`` 补齐。
    """
    book = errors if errors is not None else ErrorBook()
    policy = DEFAULT_FILL_POLICY if fill_policy is None else fill_policy
    for source, meta in observations.items():
        _validate_terms(meta, source, book)

    chosen: dict[str, str | None] = {}
    for field in FIELDS:
        per_source = {
            name: getattr(meta, field)
            for name, meta in sorted(observations.items())
            if getattr(meta, field) is not None
        }
        distinct = set(per_source.values())
        if len(distinct) > 1:
            book.add(
                ErrorCode.COLOR_CONFLICT,
                f"色彩字段 {field} 存在冲突: {per_source}",
                source=",".join(sorted(per_source)),
                fatal=False,
                detail={"field": field, "sources": per_source},
            )
            picked = next(
                term for term in CONFLICT_PRIORITY[field] if term in distinct
            )
        elif distinct:
            picked = next(iter(distinct))
        else:
            picked = None
        chosen[field] = picked

    filled = fill_missing(
        ColorMetadata(**chosen), policy, source="reconcile", errors=book
    )
    return filled
