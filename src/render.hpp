// 离屏帧导出：按时间线采样，输出 PNG 帧序列与帧清单（无任何窗口/显示依赖）。
// 帧清单逐帧记录统一时间刻度、源位置、SHA-256，并明确处理缺帧、重复采样
// 以及最后一个不足整帧的尾区间。
#pragma once
#include <cstdint>
#include <filesystem>
#include <string>
#include <vector>

#include "media.hpp"
#include "timeline.hpp"
#include "util.hpp"

namespace mb {

struct RenderFrame {
  std::string file;             // 相对输出目录的 PNG 文件名
  int64_t master_ticks = 0;     // 统一主刻度采样点
  int64_t frame_index = 0;      // 对应源媒体的整数帧号
  int64_t source_offset = -1;   // 源位置（时间线片段的源偏移）
  std::string sha256;
  std::string track_id;
};

struct RenderConfig {
  std::filesystem::path out_dir;
  MediaSpec media;              // 用于决定图案/帧率/尺寸
  const Timeline* timeline = nullptr;
  int emit_frames = 0;          // 期望导出帧数（0 表示按时间线全长取整帧）
};

struct RenderResult {
  std::vector<RenderFrame> frames;
  int64_t exported = 0;
  int64_t missing = 0;          // 落在媒体实际帧数之外的缺帧
  int64_t duplicates = 0;       // 同一源帧被重复采样的次数
  int64_t partial_tail = 0;     // 末尾不足一整帧的区间数
  std::filesystem::path manifest_path;
};

// 生成 PNG 帧序列并写清单 manifest.csv（含 SHA-256）。
bool render_frames(const RenderConfig& cfg, RenderResult* out, ErrList* errs);

inline constexpr const char* kRenderMissing = "RENDER_MISSING_FRAME";
inline constexpr const char* kRenderDuplicate = "RENDER_DUPLICATE_SAMPLE";
inline constexpr const char* kRenderPartial = "RENDER_PARTIAL_TAIL";
inline constexpr const char* kRenderWrite = "RENDER_WRITE_FAILED";

}  // namespace mb
