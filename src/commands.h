// commands.h — 三个子命令的处理器声明，main 仅负责参数解析与退出码映射
#pragma once

#include <string>
#include <vector>

#include "sample_model.h"

namespace popfix {

// 各处理器返回 ExitCode；错误信息写入 stderr，正常摘要写入 stdout。
ExitCode cmd_generate_fixture(const std::vector<std::string>& tokens);
ExitCode cmd_analyze(const std::vector<std::string>& tokens);
ExitCode cmd_verify(const std::vector<std::string>& tokens);

}  // namespace popfix
