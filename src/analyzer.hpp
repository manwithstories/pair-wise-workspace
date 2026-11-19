#pragma once
// 单文件分析引擎：解复用→有界队列（背压）→解码分析→定位/同步→指标汇总。
#include "error_model.hpp"
#include "stream_pipeline.hpp"

#include <string>

namespace ma {

struct AnalyzeOptions {
    std::string input;
    size_t memoryMiB = 64;
    int timeoutMs = 30000;       // 超时（ms），0 表示不限制
    int queueItemLimit = 256;    // 队列条目上限（另一道有界约束）
    uint64_t seed = 42;
};

// 执行完整分析；返回 false 表示内部错误（退出码 4），结果内 ok=false 表示
// 输入/探测问题（退出码 2），阈值判定由调用方用 AuditResult::evaluate 完成。
bool analyzeFile(const AnalyzeOptions& opt, AuditResult& out,
                 std::string& err);

} // namespace ma
