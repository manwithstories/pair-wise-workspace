// 批处理编排：把字幕/时间码 -> 时间线 -> 有界队列流 -> 分块检查点
// -> FFmpeg 再封装 -> 离屏导出 -> 报告 串成一条有背压、可续跑的管线。
#pragma once
#include <cstdint>
#include <filesystem>
#include <string>

#include "checkpoint.hpp"
#include "fixtures.hpp"
#include "report.hpp"

namespace mb {

struct PipelineConfig {
  std::filesystem::path work_dir;
  bool damaged = false;
  bool resume = false;
  int64_t target_bitrate_bps = 4'000'000;
  std::string container = "matroska";
  int64_t chunk_frames = 120;
  size_t queue_capacity = 8;
  int64_t slow_consumer_ms = 0;
  int emit_frames = 12;
};

struct PipelineOutcome {
  ReportInputs report;
  CpError checkpoint_error;  // 空表示无检查点错误
  std::string fatal;         // 非空表示构建/运行级致命错误（退出码 5）
  int exit_code = 0;
};

// 运行完整管线；返回退出语义由 report.decide_exit_code 统一裁定。
PipelineOutcome run_pipeline(const PipelineConfig& cfg);

}  // namespace mb
