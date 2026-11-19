// 报告聚合：消费各模块的统计与索引，产出终端 JSON 与限值判定。
// 退出语义：0 合格；2 含可恢复错误但报告已生成；3 检查点/输入校验失败；
//           4 输出指标超限；5 构建或参数错误。
#pragma once
#include <cstdint>
#include <map>
#include <string>
#include <vector>

#include "checkpoint.hpp"
#include "media.hpp"
#include "render.hpp"
#include "stream.hpp"
#include "subtitle.hpp"
#include "util.hpp"

namespace mb {

// 判定限值（可由命令行覆盖；带默认值）。
struct ReportLimits {
  double max_bitrate_deviation_pct = 5.0;
  int64_t max_subtitle_alignment_ms = 40;
  double min_video_psnr_db = 30.0;
};

struct ReportInputs {
  int64_t target_bitrate_bps = 0;
  double actual_bitrate_bps = 0.0;
  double video_psnr_db = 0.0;
  double audio_peak_dbfs = -99.0;
  int64_t subtitle_alignment_max_ms = 0;
  int64_t frames_exported = 0;
  int64_t frame_sha256_count = 0;
  size_t queue_capacity = 0;
  size_t queue_peak_items = 0;
  uint64_t queue_peak_bytes = 0;
  uint64_t queue_push_waits = 0;
  uint64_t peak_memory_bytes = 0;
  size_t checkpoint_resumed_blocks = 0;
  size_t checkpoint_failed_blocks = 0;
  ErrList errors;                    // 全链路错误（含字幕/媒体/渲染/时间线）
  std::map<std::string, int> extra;  // 附加计数（如损坏记录数）
  std::string first_error_location;
  int64_t first_error_offset = -1;
  ReportLimits limits;
};

struct ReportResult {
  std::string json;
  std::map<std::string, int> errors_by_code;
  bool metrics_ok = true;
  bool has_recoverable_errors = false;
};

// 汇总并判定。cp_failure 非空表示检查点/输入校验失败（退出码 3）。
ReportResult build_report(const ReportInputs& in, const CpError* cp_failure);

// 字幕对齐误差：每条字幕起点对齐到最近视频帧的最大毫秒偏差。
int64_t max_subtitle_alignment_ms(const SubtitleDoc& doc,
                                  const Ratio& video_fps);

// 退出码判定（集中在一处，保证 CLI 与测试一致）。
int decide_exit_code(const ReportResult& rr, const CpError* cp_failure,
                     bool arg_error);

}  // namespace mb
