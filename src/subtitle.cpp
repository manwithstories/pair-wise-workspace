#include "subtitle.hpp"

#include <algorithm>
#include <cstring>

#include "timebase.hpp"

namespace mb {

namespace {
// 去掉行尾 CR 与首尾空白，返回裁剪后的视图在原串中的 [begin,end)。
std::string trim(const std::string& s) {
  size_t b = 0, e = s.size();
  while (b < e && (s[b] == ' ' || s[b] == '\t' || s[b] == '\r')) ++b;
  while (e > b && (s[e - 1] == ' ' || s[e - 1] == '\t' || s[e - 1] == '\r'))
    --e;
  return s.substr(b, e - b);
}

// 在一行内定位时间轴 " --> "，返回起点位置；找不到返回 npos。
size_t find_arrow(const std::string& line) {
  return line.find(" --> ");
}

// VTT 时间轴可能带尾部 " settings:value"，先截到箭头后第二段时间为止。
bool split_cue_line(const std::string& line, std::string& a, std::string& b) {
  size_t ar = find_arrow(line);
  if (ar == std::string::npos) return false;
  a = trim(line.substr(0, ar));
  std::string tail = trim(line.substr(ar + 5));
  // 第二段时间到第一个空白为止（其后是 VTT 设置项）。
  size_t sp = tail.find_first_of(" \t");
  b = sp == std::string::npos ? tail : tail.substr(0, sp);
  return true;
}
}  // namespace

SubFormat detect_sub_format(const std::string& bytes) {
  if (bytes.rfind("WEBVTT", 0) == 0) return SubFormat::kVtt;
  return SubFormat::kSrt;
}

SubtitleDoc parse_subtitles(const std::string& bytes, SubFormat fmt) {
  SubtitleDoc doc;
  if (fmt == SubFormat::kAuto) fmt = detect_sub_format(bytes);

  // 按行切分，同时累计字节偏移（按原始字节，含换行符宽度）。
  std::vector<std::pair<size_t, std::string>> lines;  // (行首字节偏移, 行)
  size_t off = 0;
  size_t i = 0;
  while (i <= bytes.size()) {
    size_t j = bytes.find('\n', i);
    if (j == std::string::npos) j = bytes.size();
    lines.emplace_back(off, bytes.substr(i, j - i));
    off += (j - i) + (j < bytes.size() ? 1 : 0);  // 含 '\n'
    i = j + 1;
    if (j == bytes.size()) break;
  }

  size_t idx = 0;
  // 跳过 VTT 头部块（WEBVTT 行、空行、NOTE/STYLE/REGION 块）。
  auto is_blank = [](const std::string& l) { return trim(l).empty(); };
  if (fmt == SubFormat::kVtt) {
    while (idx < lines.size() && is_blank(lines[idx].second)) ++idx;
    if (idx < lines.size() && lines[idx].second.rfind("WEBVTT", 0) == 0)
      ++idx;
    while (idx < lines.size()) {
      if (is_blank(lines[idx].second)) { ++idx; continue; }
      const std::string& l = trim(lines[idx].second);
      if (l.rfind("NOTE", 0) == 0 || l.rfind("STYLE", 0) == 0 ||
          l.rfind("REGION", 0) == 0) {
        ++idx;
        while (idx < lines.size() && !is_blank(lines[idx].second)) ++idx;
        continue;
      }
      break;
    }
  }

  while (idx < lines.size()) {
    if (is_blank(lines[idx].second)) { ++idx; continue; }
    size_t cue_off = lines[idx].first;
    // SRT 序号行（纯数字）；VTT 可选 cue 标识（非时间轴的任意单行）。
    std::string first = trim(lines[idx].second);
    bool first_is_time = find_arrow(first) != std::string::npos;
    if (!first_is_time) {
      bool numeric = !first.empty() &&
          std::all_of(first.begin(), first.end(),
                      [](char c) { return c >= '0' && c <= '9'; });
      if (fmt == SubFormat::kSrt && !numeric) {
        doc.errors.push_back({kErrSubFmt, "expected numeric index, got: " +
                                first, (int64_t)cue_off});
        ++idx;
        while (idx < lines.size() && !is_blank(lines[idx].second)) ++idx;
        doc.skipped_count++;
        continue;
      }
      ++idx;  // VTT cue id / SRT 序号：消费掉，下一行应为时间轴
      if (idx >= lines.size() || is_blank(lines[idx].second)) {
        doc.errors.push_back({kErrSubTruncated,
                              "cue header without timing line",
                              (int64_t)cue_off});
        doc.skipped_count++;
        continue;
      }
    }
    size_t time_off = lines[idx].first;
    std::string tline = trim(lines[idx].second);
    std::string sa, sb;
    if (!split_cue_line(tline, sa, sb)) {
      doc.errors.push_back({kErrSubTruncated, "malformed timing line: " +
                              tline, (int64_t)time_off});
      ++idx;
      while (idx < lines.size() && !is_blank(lines[idx].second)) ++idx;
      doc.skipped_count++;
      continue;
    }
    auto ta = parse_ms_timestamp(sa);
    auto tb = parse_ms_timestamp(sb);
    if (!ta || !tb || *tb < *ta) {
      doc.errors.push_back({kErrSubTime,
          std::string("invalid cue time: ") + sa + " --> " + sb,
          (int64_t)time_off});
      ++idx;
      while (idx < lines.size() && !is_blank(lines[idx].second)) ++idx;
      doc.skipped_count++;
      continue;
    }
    ++idx;
    // 收集文本直到空行。
    std::string text;
    bool truncated = false;
    while (idx < lines.size() && !is_blank(lines[idx].second)) {
      std::string l = lines[idx].second;
      if (!l.empty() && l.back() == '\r') l.pop_back();
      if (!text.empty()) text.push_back('\n');
      text += l;
      ++idx;
      if (text.size() > kMaxSubtitleText) { truncated = true; text.resize(
          kMaxSubtitleText); break; }
    }
    if (truncated) {
      doc.errors.push_back({kErrSubTextLong, "cue text exceeds limit",
                            (int64_t)time_off});
    }
    if (doc.cues.size() >= kMaxSubtitleEntries) {
      doc.errors.push_back({kErrSubTooMany, "subtitle entry limit reached",
                            (int64_t)time_off});
      doc.bounded_abort = true;
      break;
    }
    SubCue cue;
    cue.start_ticks = *ta;
    cue.end_ticks = *tb;
    cue.text = text;
    cue.source_offset = (int64_t)time_off;
    cue.sequence = doc.cues.size();
    // 与上一条比较：重复时间戳 / 重叠（仅记录，不丢条目）。
    if (!doc.cues.empty()) {
      const SubCue& prev = doc.cues.back();
      if (prev.start_ticks == cue.start_ticks &&
          prev.end_ticks == cue.end_ticks) {
        doc.duplicate_count++;
        doc.errors.push_back({kErrSubDuplicate, "duplicate cue timestamps",
                              cue.source_offset});
      } else if (cue.start_ticks < prev.end_ticks &&
                 cue.start_ticks >= prev.start_ticks) {
        doc.overlap_count++;
        doc.errors.push_back({kErrSubOverlap, "overlapping cue",
                              cue.source_offset});
      }
    }
    doc.duration_ticks = std::max(doc.duration_ticks, cue.end_ticks);
    doc.cues.push_back(std::move(cue));
  }
  return doc;
}

}  // namespace mb
