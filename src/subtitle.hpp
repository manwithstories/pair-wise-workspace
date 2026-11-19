// 字幕解析：SRT / WebVTT -> 统一主刻度的字幕条目，保留源字节偏移。
// 不可恢复记录写入错误清单后跳过，继续解析后续有效条目。
#pragma once
#include <cstdint>
#include <string>
#include <vector>

#include "util.hpp"

namespace mb {

enum class SubFormat { kAuto, kSrt, kVtt };

struct SubCue {
  int64_t start_ticks = 0;  // 统一主刻度（90kHz）
  int64_t end_ticks = 0;
  std::string text;         // 多行文本以 '\n' 连接，已去除行尾 CR
  int64_t source_offset = -1;  // 时间轴行在源文件中的字节偏移
  size_t sequence = 0;         // 文件内序号（0 起，仅统计有效条目）
};

struct SubtitleDoc {
  std::vector<SubCue> cues;
  ErrList errors;
  bool bounded_abort = false;   // 触及硬性上限（条目数/文本长度），有界失败
  int64_t duration_ticks = 0;   // 最后一条结束时间
  size_t overlap_count = 0;
  size_t duplicate_count = 0;
  size_t skipped_count = 0;     // 被跳过的不可恢复记录数
};

// 解析内存中的字幕字节串。fmt=kAuto 时按 WEBVTT 魔数判别。
SubtitleDoc parse_subtitles(const std::string& bytes,
                            SubFormat fmt = SubFormat::kAuto);
SubFormat detect_sub_format(const std::string& bytes);

// 稳定错误码（写入报告 errors_by_code）。
inline constexpr const char* kErrSubTime = "SUB_INVALID_TIME";
inline constexpr const char* kErrSubTruncated = "SUB_TRUNCATED_LINE";
inline constexpr const char* kErrSubOverlap = "SUB_OVERLAP";
inline constexpr const char* kErrSubDuplicate = "SUB_DUPLICATE_TIME";
inline constexpr const char* kErrSubTextLong = "SUB_TEXT_TOO_LONG";
inline constexpr const char* kErrSubTooMany = "SUB_TOO_MANY_ENTRIES";
inline constexpr const char* kErrSubFmt = "SUB_FORMAT_UNKNOWN";

}  // namespace mb
