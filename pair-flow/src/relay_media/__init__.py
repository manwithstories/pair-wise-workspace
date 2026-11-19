"""relay_media — 边缘媒体片段可靠转发工具链。

模块划分：
- timeline: 整数微秒时间基准与跨模块错误模型
- bitstream: H.264/H.265/AV1 裸码流分块扫描
- keyframe_index: 关键帧索引校验/补建
- segmenter: 关键帧与时间窗口切片
- silence: PCM/WAV 静音检测
- fingerprint: 感知指纹与去重
- transport: 丢包/乱序/重复/重连模拟
- pipeline: 分阶段编排与检查点
- report: JSON 报告与终端摘要
- cli: 命令行入口与退出码
"""

__version__ = "0.1.0"
