#pragma once
// 跨模块共用的错误分类与恢复记录（完整实现见第 2 批提交）。
#include <cstdint>
#include <string>
#include <vector>

namespace ma {

enum class ErrorCategory {
    None,
    DecodeFailed,   // 解码失败 / 损坏包
    Truncated,      // 文件截断 / IO 提前结束
    CorruptPacket,  // 容器层损坏包
    Timestamp,      // 时间戳异常
};

} // namespace ma
