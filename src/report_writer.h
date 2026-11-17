// report_writer.h — JSON 报告落盘、稳定排序终端摘要、报告回读
//
// 报告写入与修复音频写出共享调用方传入的同一份 RepairResult。
// JSON 为手写最小序列化（无第三方依赖），事件按 event_id 稳定排序。
#pragma once

#include <string>
#include <vector>

#include "sample_model.h"
#include "wav_io.h"

namespace popfix {

// 报告回读结构（仅 verify 实际需要的字段）。
struct StoredReport {
  RepairStats stats;
  std::vector<EventRepair> repairs;
  std::uint32_t sample_rate = 0;
  std::uint16_t channels = 0;
  std::size_t frames = 0;
  double threshold = 0.0;
  std::size_t context = 0;
};

// 原子写出 JSON 报告（临时文件 + rename）。失败抛 WavIOError(写失败)。
void write_report(const std::string& path, const RepairResult& result,
                  const DetectorConfig& dcfg, const RepairConfig& rcfg,
                  bool dry_run);

// 终端稳定排序摘要行：events=... repaired_frames=... 等，键序固定。
std::vector<std::string> build_summary_lines(const RepairResult& result,
                                             std::size_t frames);

// 解析本工具写出的报告。解析失败抛 WavIOError（读失败，退出码 2）。
StoredReport read_report(const std::string& path);

}  // namespace popfix
