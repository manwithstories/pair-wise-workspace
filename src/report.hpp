#pragma once
// 结果输出：单条 JSON（含中文字段说明）、JSON Lines 批处理与终端摘要。
// 输出文件与输入路径相同时拒绝写入（磁盘输出不得覆盖输入）。
#include "error_model.hpp"

#include <string>
#include <vector>

namespace ma {

// 将单条结果序列化为紧凑 JSON（字段名对齐验收指标）。
std::string toJson(const AuditResult& r);

// 追加一行 JSON 到 jsonlPath（不存在则创建）；禁止与输入同路径。
bool appendJsonLine(const std::string& jsonlPath,
                    const std::string& inputPath,
                    const std::string& line, std::string& err);

// 终端可读摘要（纯文本，无 GUI 依赖）。
std::string toTerminalSummary(const AuditResult& r,
                              const Thresholds& th);

// 批处理汇总行。
std::string toBatchSummary(size_t total, size_t failed,
                           size_t thresholdMissed);

} // namespace ma
