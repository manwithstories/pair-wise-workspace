#pragma once
// 对外共享声明（逐批填充实现）。
#include <string>
#include <vector>

namespace ma {

// 退出码约定：0 达标；2 参数/输入错误；3 完成但指标未达阈值；4 内部错误。
enum class ExitCode : int {
    Ok = 0,
    Usage = 2,
    Threshold = 3,
    Internal = 4,
};

} // namespace ma
