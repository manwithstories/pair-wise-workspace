"""region_media —— 跨节点 HLS/DASH 传输媒体质量核验。

模块职责：
    model      统一媒体时钟、分片状态与错误模型
    manifest   HLS/DASH 清单解析与引用校验
    ingest     PyAV 解复用、回绕/断流识别与网络条件注入
    clock      节点漂移估计与音视频时间基准映射
    buffer     自适应抖动缓冲与乱序窗口
    recovery   丢包/重复分片/可恢复缺口处理
    sync       音画对齐与可变帧率同步评估
    checkpoint SQLite 事务检查点与中断续跑
    metrics    恢复率、同步误差、P95 延迟汇总
    generator  本地样例夹具生成
    cli        命令行入口与退出码汇总
"""

from pkgutil import extend_path

__path__ = extend_path(__path__, __name__)
__version__ = "0.1.0"
