#pragma once

#include <filesystem>
#include <string>
#include <system_error>

#include "mediacore/error.hpp"

namespace mediacore {

// 路径安全：所有输入/输出必须落在 baseDir 之内（词法规范化后比较）。
// 拒绝 ".." 穿越、绝对路径逃逸与符号链接逃逸，返回 PathTraversal 错误。
Result<std::filesystem::path>
safeJoin(const std::filesystem::path& baseDir,
         const std::filesystem::path& userPath) noexcept;

// 确保目录存在（递归创建），失败返回可诊断错误。
Result<std::filesystem::path>
ensureDirectory(const std::filesystem::path& dir) noexcept;

} // namespace mediacore
