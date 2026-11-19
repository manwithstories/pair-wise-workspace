// pipeline_io.hpp — 流水线的片段落盘、检查点校验与指标辅助
#pragma once
#include "common.hpp"
#include "checkpoint.hpp"
#include <string>
#include <vector>

namespace md {

struct PipelineHooks; // 定义于 pipeline.hpp

// 片段临时目录（out 同目录，原子提交，不覆盖源包流）
std::string segment_dir(const ProcessConfig& cfg);
std::string segment_blob_path(const ProcessConfig& cfg, uint32_t id);

// 原子落盘片段；命中 io_fail_path 时模拟磁盘写失败
bool write_segment_blob(const ProcessConfig& cfg, const PipelineHooks& hooks,
                        uint32_t id, const std::vector<uint8_t>& bytes,
                        PipelineError& err);

// 校验已落盘片段哈希；文件缺失或哈希不符返回 false 并记录可追溯错误
bool verify_segment_blob(const ProcessConfig& cfg, const SegmentCheckpoint& sc,
                         std::vector<PipelineError>& errors);

// 检查点兼容性：不兼容返回对应退出码（4），兼容返回 Ok
ExitCode checkpoint_compat(const ProcessConfig& cfg,
                           const std::array<uint8_t, 32>& input_sha,
                           const std::array<uint8_t, 32>& params_sha,
                           CheckpointData& cp, bool& had_checkpoint);

// 模拟中断（测试恢复）：通过异常跳出处理循环，main 转为专用退出码
struct CrashInjected {
    uint32_t after_segment;
};

} // namespace md
