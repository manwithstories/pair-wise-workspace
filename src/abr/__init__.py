"""ABR 自适应码率决策引擎。

模块划分：
    types      不可变数据结构与可诊断异常
    trace      可复现的网络轨迹生成（带宽/延迟/丢包/突发抖动）
    estimator  滑动窗口 + 指数平滑 + 异常值抑制的带宽估计器
    buffer     分段下载、播放消耗、缓冲增长、下溢恢复模拟
    policy     基于收益/风险的档位决策
    controller 编排估计、缓冲模拟与决策（滞后/冷却/恢复）
    metrics    全过程事件统计
    cli        trace / simulate / compare 子命令
"""

__version__ = "1.0.0"
