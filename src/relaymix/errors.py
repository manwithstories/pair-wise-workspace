"""结构化错误模型。

时间轴、合成、导出与恢复模块共用同一错误结构，CLI 负责把它序列化为
固定字段名的 JSON 行：

    {"error_code": ..., "message": ..., "source_block": ..., "byte_offset": ...}

退出码约定：
    0  处理或核验成功
    2  清单或参数无效（ManifestError）
    3  素材损坏（MediaError）
    4  检查点或溯源校验失败（CheckpointError）
"""

import json
from typing import Any

EXIT_OK = 0
EXIT_MANIFEST = 2
EXIT_MEDIA = 3
EXIT_CHECKPOINT = 4


class RelayError(Exception):
    """所有可预期业务错误的基类。"""

    exit_code = 1
    error_code = "relay_error"

    def __init__(self, message: str, **fields: Any) -> None:
        super().__init__(message)
        # 允许实例级覆盖 error_code，便于细分类别。
        code = fields.pop("error_code", None)
        if code is not None:
            self.error_code = code
        self.fields: dict[str, Any] = fields

    def to_dict(self) -> dict[str, Any]:
        data: dict[str, Any] = {
            "error_code": self.error_code,
            "message": str(self),
        }
        # 固定字段排序，保证输出确定且字段名稳定。
        for key in sorted(self.fields):
            data[key] = self.fields[key]
        return data

    def to_jsonline(self) -> str:
        return json.dumps(self.to_dict(), ensure_ascii=False, sort_keys=True)


class ManifestError(RelayError):
    """清单或参数无效（负长度、未知轨道、非单调、越界、零采样率等）。"""

    exit_code = EXIT_MANIFEST
    error_code = "invalid_manifest"


class MediaError(RelayError):
    """素材损坏（截断头、数据长度不符、非法声道、空块、布局不支持等）。"""

    exit_code = EXIT_MEDIA
    error_code = "media_error"


class CheckpointError(RelayError):
    """检查点 / 指纹 / 溯源校验失败。"""

    exit_code = EXIT_CHECKPOINT
    error_code = "checkpoint_error"
