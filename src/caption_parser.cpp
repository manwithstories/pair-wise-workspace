#include "relay/caption_parser.hpp"

#include <charconv>
#include <optional>

namespace relay {
namespace {

struct Line {
  std::string_view text;
  uint64_t offset = 0;  // byte offset of line start in the source
};

// Split into lines, tolerating a final line without a trailing newline.
std::vector<Line> split_lines(std::string_view text) {
  std::vector<Line> out;
  size_t pos = 0;
  while (pos < text.size()) {
    size_t nl = text.find('\n', pos);
    size_t end = nl == std::string_view::npos ? text.size() : nl;
    std::string_view line = text.substr(pos, end - pos);
    if (!line.empty() && line.back() == '\r') line.remove_suffix(1);
    out.push_back({line, static_cast<uint64_t>(pos)});
    pos = end + 1;
  }
  if (pos == text.size() && !text.empty() && text.back() == '\n') {
    // trailing newline: no extra empty record needed
  }
  return out;
}

std::string_view trim(std::string_view s) {
  while (!s.empty() && (s.front() == ' ' || s.front() == '\t')) s.remove_prefix(1);
  while (!s.empty() && (s.back() == ' ' || s.back() == '\t')) s.remove_suffix(1);
  return s;
}

bool parse_uint(std::string_view s, int64_t& out) {
  if (s.empty()) return false;
  int64_t v = 0;
  auto [ptr, ec] = std::from_chars(s.data(), s.data() + s.size(), v);
  if (ec != std::errc() || ptr != s.data() + s.size()) return false;
  out = v;
  return true;
}

// "HH:MM:SS,mmm" (SRT) or "[HH:]MM:SS.mmm" (WebVTT). Validates field ranges.
std::optional<int64_t> parse_timestamp(std::string_view ts, char ms_sep) {
  ts = trim(ts);
  int64_t hh = 0, mm = 0, ss = 0, ms = 0;
  size_t sep = ts.find(ms_sep);
  if (sep == std::string_view::npos) return std::nullopt;
  std::string_view hms = ts.substr(0, sep);
  std::string_view ms_part = ts.substr(sep + 1);
  if (ms_part.size() != 3 || !parse_uint(ms_part, ms) || ms > 999)
    return std::nullopt;
  // Split hms on ':' into 2 or 3 components.
  int64_t parts[3];
  int n = 0;
  size_t start = 0;
  while (true) {
    size_t colon = hms.find(':', start);
    std::string_view piece =
        colon == std::string_view::npos ? hms.substr(start)
                                        : hms.substr(start, colon - start);
    if (n >= 3 || piece.empty() || !parse_uint(piece, parts[n])) return std::nullopt;
    ++n;
    if (colon == std::string_view::npos) break;
    start = colon + 1;
  }
  if (n == 3) {
    hh = parts[0]; mm = parts[1]; ss = parts[2];
  } else if (n == 2 && ms_sep == '.') {
    mm = parts[0]; ss = parts[1];
  } else {
    return std::nullopt;
  }
  if (mm > 59 || ss > 59 || hh > 99) return std::nullopt;
  return ((hh * 60 + mm) * 60 + ss) * 1000 + ms;
}

// Find " --> " and parse both sides. Returns false when the line is not a
// usable timing line.
bool parse_timing_line(std::string_view line, char ms_sep, Cue& cue) {
  size_t arrow = line.find("-->");
  if (arrow == std::string_view::npos) return false;
  std::string_view left = trim(line.substr(0, arrow));
  std::string_view right = trim(line.substr(arrow + 3));
  // WebVTT allows cue settings after the end time; drop them.
  size_t sp = right.find_first_of(" \t");
  std::string_view right_ts = sp == std::string_view::npos ? right : right.substr(0, sp);
  auto start = parse_timestamp(left, ms_sep);
  auto end = parse_timestamp(right_ts, ms_sep);
  if (!start || !end) return false;
  cue.raw_start = std::string(left);
  cue.raw_end = std::string(right_ts);
  cue.start_ms = *start;
  cue.end_ms = *end;
  return true;
}

void note_anomaly(CaptionParseResult& r, std::string message,
                  std::optional<TimeRangeMs> range) {
  AnalysisError e;
  e.severity = ErrorSeverity::Recoverable;
  e.kind = DamageKind::CaptionAnomaly;
  e.message = std::move(message);
  e.range = range;
  r.anomalies.push_back(std::move(e));
}

TimeRangeMs ms_range(int64_t a, int64_t b) {
  return TimeRangeMs{{a, 1}, {b, 1}};
}

CaptionParseResult parse_blocks(std::string_view text, CaptionFormat fmt,
                                char ms_sep, size_t first_line) {
  CaptionParseResult result;
  result.format = fmt;
  auto lines = split_lines(text);
  int64_t prev_end = -1;
  uint32_t ordinal = 0;
  size_t i = first_line;
  while (i < lines.size()) {
    // Skip blank lines and (SRT) numeric index lines.
    std::string_view line = trim(lines[i].text);
    if (line.empty()) { ++i; continue; }
    int64_t ignored = 0;
    if (fmt == CaptionFormat::Srt && parse_uint(line, ignored)) { ++i; continue; }
    if (fmt == CaptionFormat::WebVtt &&
        (line.starts_with("NOTE") || line.starts_with("STYLE") || line.starts_with("REGION"))) {
      // Skip metadata block up to the next blank line.
      while (i < lines.size() && !trim(lines[i].text).empty()) ++i;
      continue;
    }

    uint64_t block_offset = lines[i].offset;
    // A WebVTT cue may carry an identifier line before the timing line.
    if (line.find("-->") == std::string_view::npos) {
      if (i + 1 < lines.size() &&
          lines[i + 1].text.find("-->") != std::string_view::npos) {
        ++i;
        line = trim(lines[i].text);
      } else {
        note_anomaly(result, "unparseable_cue_header", std::nullopt);
        while (i < lines.size() && !trim(lines[i].text).empty()) ++i;
        continue;
      }
    }

    Cue cue;
    cue.byte_offset = block_offset;
    cue.index = ++ordinal;
    if (!parse_timing_line(line, ms_sep, cue)) {
      note_anomaly(result, "invalid_cue_timing", std::nullopt);
      while (i < lines.size() && !trim(lines[i].text).empty()) ++i;
      continue;
    }
    if (cue.end_ms <= cue.start_ms)
      note_anomaly(result, "non_positive_duration",
                   ms_range(cue.start_ms, cue.end_ms));
    if (prev_end >= 0 && cue.start_ms < prev_end)
      note_anomaly(result, "overlapping_cues",
                   ms_range(cue.start_ms, prev_end));
    prev_end = cue.end_ms > prev_end ? cue.end_ms : prev_end;
    result.cues.push_back(std::move(cue));
    // Consume the cue payload up to the next blank line (or EOF, which also
    // covers a truncated final line without newline).
    while (i < lines.size() && !trim(lines[i].text).empty()) ++i;
  }
  return result;
}

}  // namespace

CaptionParseResult parse_srt(std::string_view text) {
  return parse_blocks(text, CaptionFormat::Srt, ',', 0);
}

CaptionParseResult parse_webvtt(std::string_view text) {
  size_t first = 0;
  auto lines = split_lines(text);
  if (!lines.empty() && trim(lines[0].text).starts_with("WEBVTT")) first = 1;
  return parse_blocks(text, CaptionFormat::WebVtt, '.', first);
}

CaptionParseResult parse_captions(std::string_view text) {
  if (text.starts_with("WEBVTT")) return parse_webvtt(text);
  return parse_srt(text);
}

}  // namespace relay
