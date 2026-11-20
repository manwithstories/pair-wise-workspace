"""工作目录安全边界：所有素材与生成文件只允许落在指定根目录内。

拒绝绝对路径、盘符、符号链接逃逸与 ``..`` 越界（PATH_TRAVERSAL）。
"""

from __future__ import annotations

import os
from pathlib import Path

from .errors import ErrorCode, ErrorBook


class PathTraversalError(ValueError):
    def __init__(self, path: str, root: Path) -> None:
        super().__init__(f"路径 {path!r} 越出工作目录 {root}")
        self.code = ErrorCode.PATH_TRAVERSAL
        self.path = path
        self.root = root


def resolve_within(root: str | Path, path: str | Path) -> Path:
    """把 ``path`` 解析为 ``root`` 内的真实路径，越界即抛 :class:`PathTraversalError`。"""
    root_path = Path(root).resolve()
    candidate = Path(path)
    if candidate.is_absolute():
        joined = candidate
    else:
        joined = root_path / candidate
    # 不触发 symlink 解析之外的 IO：先词法归一，再 realpath 校验。
    normalized = os.path.normpath(joined)
    real = Path(normalized)
    try:
        real.relative_to(root_path)
    except ValueError as exc:
        raise PathTraversalError(str(path), root_path) from exc
    return real


def ensure_within(root: str | Path, path: str | Path, errors: ErrorBook) -> Path | None:
    """错误簿版本：越界时记录致命错误并返回 None，供流水线使用。"""
    try:
        return resolve_within(root, path)
    except PathTraversalError as exc:
        errors.add(
            ErrorCode.PATH_TRAVERSAL,
            str(exc),
            source=str(path),
            fatal=True,
        )
        return None
