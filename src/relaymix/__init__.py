"""relaymix —— 跨节点 PCM/WAV 多轨时间线重建与离屏帧序列导出。

仅依赖本地文件与 SQLite，时间基准、结构化错误模型被时间轴、合成、
导出与恢复模块共用。
"""

# 算法版本：合成 / 重采样 / 量化 / 哈希规则发生不兼容变化时必须递增。
# 检查点恢复时据此拒绝混用旧片段。
ALGORITHM_VERSION = "relaymix-algo-1.0.0"

# 固定随机种子：所有夹具与任何随机过程都使用该种子，保证同参同出。
DETERMINISTIC_SEED = 20261120

__all__ = ["ALGORITHM_VERSION", "DETERMINISTIC_SEED"]
