"""fieldindex：现场字幕/SMPTE 时间码修复与可随机定位的分段素材索引。

数据流：字节流 -> 解析（保留原始偏移）-> 统一有理数时间基准 ->
修复（回绕/漂移/冲突）-> 分段索引 -> 回读复核（SHA-256 证据）。
"""
from __future__ import annotations

__version__ = "0.1.0"

#: 索引文件魔数与版本；版本不符或尾部损坏时只重建受影响分段。
INDEX_MAGIC = "fieldindex-jsonl"
INDEX_VERSION = 1

#: 统一时间基准上限（秒）；输入换算结果越界即记错误并隔离。
MAX_TIMELINE_SECONDS = 24 * 3600
