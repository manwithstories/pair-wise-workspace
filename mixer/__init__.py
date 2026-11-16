"""多来源素材时间线双声道混音库。

模块划分:
- types:      数据类型、错误层级
- config:     project.json 解析与字段校验
- resample:   确定性加窗 sinc 重采样
- automation: 分段线性自动化曲线
- graph:      声道映射、增益与求和
- metrics:    峰值/RMS/削波统计
- stream:     块式混音引擎(离线与流式共享)
- offline:    整段离线处理与 WAV/JSON 落盘
- fixtures:   本地夹具与样例工程生成
"""

__all__ = [
    "types",
    "resample",
    "automation",
    "graph",
    "metrics",
    "stream",
    "offline",
    "fixtures",
]
