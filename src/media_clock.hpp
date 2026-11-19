#pragma once
// 统一媒体时间基准：回绕展开、异常时间戳识别（完整实现见第 2 批提交）。
#include <cstdint>
#include <optional>
#include <vector>

namespace ma {

// 时间戳异常分类（初步定义，随模块演进扩展）。
enum class TsKind {
    Normal,
    Wrap,       // 检测到回绕并已展开
    Repeat,     // 与上一帧相同
    Backward,   // 非回绕性倒退
    Missing,    // 间隔显著超出预期
};

} // namespace ma
