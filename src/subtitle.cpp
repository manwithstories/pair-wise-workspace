#include "media/subtitle.hpp"

#include "media/timebase.hpp"

#include <optional>

namespace media {
namespace {

struct Line {
    std::string text;
    int64_t number;  // 1 起
    int64_t offset;  // 字节偏移
};

std::vector<Line> split_lines(const std::string& content) {
    std::vector<Line> lines;
    size_t pos = 0;
    int64_t num = 1;
    while (pos <= content.size()) {
        size_t nl = content.find('\n', pos);
        if (nl == std::string::npos) {
            if (pos < content.size())
                lines.push_back({content.substr(pos), num,
                                 static_cast<int64_t>(pos)});
            break;
        }
        std::string text = content.substr(pos, nl - pos);
        if (!text.empty() && text.back() == '\r') text.pop_back();
        lines.push_back({std::move(text), num, static_cast<int64_t>(pos)});
        pos = nl + 1;
        num++;
    }
    return lines;
}

// "HH:MM:SS,mmm" / "HH:MM:SS.mmm" / "MM:SS.mmm" -> 主时基 tick
std::optional<int64_t> parse_timestamp(const std::string& text) {
    int h = 0, m = 0, s = 0, ms = 0;
    char sep;
    if (std::sscanf(text.c_str(), "%d:%d:%d%c%d", &h, &m, &s, &sep, &ms) == 5) {
        // 完整形式
    } else if (std::sscanf(text.c_str(), "%d:%d%c%d", &m, &s, &sep, &ms) == 4) {
        h = 0;
    } else {
        return std::nullopt;
    }
    if ((sep != ',' && sep != '.') || m > 59 || s > 59 || ms > 999)
        return std::nullopt;
    return ms_to_ticks((static_cast<int64_t>(h) * 3600 + m * 60 + s) * 1000 + ms);
}

struct Timing {
    int64_t start;
    int64_t end;
};

std::optional<Timing> parse_timing_line(const std::string& line) {
    auto arrow = line.find("-->");
    if (arrow == std::string::npos) return std::nullopt;
    auto start = parse_timestamp(line.substr(0, arrow));
    std::string rest = line.substr(arrow + 3);
    // 跳过 "-->" 后的前导空白；WebVTT 行尾可能带位置设置，按空白截断。
    auto lead = rest.find_first_not_of(" \t");
    if (lead == std::string::npos) return std::nullopt;
    rest = rest.substr(lead);
    auto sp = rest.find_first_of(" \t");
    if (sp != std::string::npos) rest = rest.substr(0, sp);
    auto end = parse_timestamp(rest);
    if (!start || !end || *end < *start) return std::nullopt;
    return Timing{*start, *end};
}

bool is_blank(const std::string& s) {
    return s.find_first_not_of(" \t") == std::string::npos;
}

} // namespace

SubtitleParseResult parse_subtitles(const std::string& content,
                                    SubtitleFormat format,
                                    OverlapPolicy policy, ErrorModel& errors,
                                    const std::string& source_track) {
    SubtitleParseResult result;
    auto lines = split_lines(content);
    size_t i = 0;
    uint32_t cue_index = 0;

    // WebVTT：跳过文件头与 NOTE/STYLE/REGION 块。
    if (format == SubtitleFormat::WebVtt) {
        if (i < lines.size() && lines[i].text.rfind("WEBVTT", 0) == 0) {
            ++i;
            while (i < lines.size() && is_blank(lines[i].text)) ++i;
        }
    }

    while (i < lines.size()) {
        if (is_blank(lines[i].text)) {
            ++i;
            continue;
        }
        // 收集一个记录块（到空行或文件尾）。
        size_t block_start = i;
        while (i < lines.size() && !is_blank(lines[i].text)) ++i;
        size_t block_end = i; // [block_start, block_end)

        const Line& first = lines[block_start];
        if (format == SubtitleFormat::WebVtt &&
            (first.text.rfind("NOTE", 0) == 0 || first.text == "STYLE" ||
             first.text == "REGION"))
            continue;

        // 定位时间轴行：SRT 首行为序号；VTT 首行可能是标识符。
        size_t timing_idx = block_start;
        if (parse_timing_line(lines[timing_idx].text) == std::nullopt) {
            if (timing_idx + 1 < block_end &&
                parse_timing_line(lines[timing_idx + 1].text) != std::nullopt) {
                timing_idx++;
            } else {
                result.bad_records++;
                errors.record({ErrorKind::CorruptRecord, source_track, -1,
                               first.number, first.offset,
                               "missing/invalid timing line, block skipped"});
                continue;
            }
        }

        auto timing = parse_timing_line(lines[timing_idx].text);
        cue_index++;
        // 截断判定：时间轴行是块内最后一行（无正文）或文件在块中途结束。
        bool truncated = timing_idx + 1 >= block_end;
        std::string text;
        for (size_t k = timing_idx + 1; k < block_end; ++k) {
            if (!text.empty()) text += '\n';
            text += lines[k].text;
        }
        if (truncated) {
            result.bad_records++;
            errors.record({ErrorKind::TruncatedRecord, source_track,
                           static_cast<int64_t>(cue_index),
                           lines[timing_idx].number, lines[timing_idx].offset,
                           "cue has timing but no payload text"});
        }
        result.cues.push_back({timing->start, timing->end, std::move(text),
                               cue_index});
    }

    // 重叠处理：按开始时间排序后应用策略。
    std::sort(result.cues.begin(), result.cues.end(),
              [](const Cue& a, const Cue& b) { return a.start_ticks < b.start_ticks; });
    if (policy != OverlapPolicy::Keep) {
        std::vector<Cue> out;
        for (auto& cue : result.cues) {
            if (!out.empty() && cue.start_ticks < out.back().end_ticks) {
                result.overlaps_resolved++;
                if (policy == OverlapPolicy::Trim) {
                    out.back().end_ticks = cue.start_ticks;
                } else { // Drop
                    continue;
                }
            }
            out.push_back(std::move(cue));
        }
        result.cues = std::move(out);
    }
    return result;
}

} // namespace media
