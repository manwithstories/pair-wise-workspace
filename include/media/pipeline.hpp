// 流水线编排：重排 -> 缓冲 -> 恢复 -> 时间码/字幕映射 -> 音画同步
// -> 检查点分块 -> 清单。支持中断续跑（按已提交块跳过输入）。
#pragma once

#include "media/demo_generator.hpp"
#include "media/error_model.hpp"
#include "media/manifest.hpp"

#include <cstdint>

namespace media {

struct PipelineResult {
    ManifestData manifest;
    bool write_failed = false;      // 检查点或清单写失败 -> 退出码 4
    bool resumed = false;           // 本次运行是否从检查点续跑
    bool completed = false;         // 是否处理完全部输入（false = 模拟中断）
    uint64_t unrecoverable = 0;     // 不可恢复包数 -> 退出码 3
};

// stop_after_arrivals 用于模拟批处理中断：处理指定到达数后停止，
// 已提交块保留，不写清单。
PipelineResult run_pipeline(const DemoConfig& cfg, const DemoTrace& trace,
                            ErrorModel& errors, uint64_t stop_after_arrivals);

} // namespace media
