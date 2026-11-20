"""输入与配置摘要：恢复时用于判定检查点是否仍然有效。"""

from __future__ import annotations

import hashlib
import json
from pathlib import Path
from typing import Any

from .checkpoint import canonical_json, sha256_file
from .safety import resolve_within


def input_digest(workdir: str | Path, rel_paths: list[str]) -> dict[str, str]:
    """对工作目录内的输入文件逐个计算 SHA-256，返回稳定映射。"""
    digest: dict[str, str] = {}
    for rel in sorted(rel_paths):
        safe = resolve_within(workdir, rel)
        digest[rel] = sha256_file(safe)
    return dict(sorted(digest.items()))


def config_digest(config: dict[str, Any]) -> str:
    """配置摘要：规范化 JSON 后取 SHA-256，保证重复运行一致。"""
    return hashlib.sha256(canonical_json(config)).hexdigest()


def write_json(path: str | Path, workdir: str | Path, payload: Any) -> Path:
    safe = resolve_within(workdir, path)
    safe.parent.mkdir(parents=True, exist_ok=True)
    safe.write_bytes(canonical_json(payload))
    return safe


def read_json(path: str | Path, workdir: str | Path) -> Any:
    safe = resolve_within(workdir, path)
    return json.loads(safe.read_bytes())
