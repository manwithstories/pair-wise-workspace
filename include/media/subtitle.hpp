// 字幕解析：SRT / WebVTT -> 统一时间基准的区间序列。
// 容忍损坏或截断的记录：报告行号与字节偏移后继续解析后续有效记录。
// 重叠字幕按可配置策略保留或裁剪。
#pragma once

#include "media/error_model.hpp"

#include <cstdint>
#include <string>
#include <vector>

namespace media {

enum class SubtitleFormat { Srt, WebVtt };
enum class OverlapPolicy { Keep, Trim, Drop };

struct Cue {
    int64_t start_ticks = 0; // 主时基
    int64_t end_ticks = 0;
    std::string text;
    uint32_t index = 0; // 源文件中的记录序号（从 1 起）
};

struct SubtitleParseResult {
    std::vector<Cue> cues;
    uint64_t bad_records = 0;    // 损坏/截断记录数
    uint64_t overlaps_resolved = 0;
};

SubtitleParseResult parse_subtitles(const std::string& content,
                                    SubtitleFormat format,
                                    OverlapPolicy policy, ErrorModel& errors,
                                    const std::string& source_track);

} // namespace media
