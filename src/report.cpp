#include "report.hpp"

#include <algorithm>
#include <cstdio>
#include <fstream>
#include <map>
#include <sstream>

namespace smx {
namespace {
void count_overflow(const LayoutResult& layout, Report& report) {
  if (layout.width_overflow) ++report.overflow_width;
  if (layout.lines_overflow) ++report.overflow_lines;
  if (layout.unbreakable) ++report.overflow_unbreakable;
  if (layout.missing_glyph) ++report.overflow_missing_glyph;
}

std::string overflow_names(const LayoutResult& layout) {
  std::string out;
  auto add = [&](const char* name) {
    if (!out.empty()) out += ",";
    out += "\"";
    out += name;
    out += "\"";
  };
  if (layout.width_overflow) add("WIDTH");
  if (layout.lines_overflow) add("LINES");
  if (layout.unbreakable) add("UNBREAKABLE");
  if (layout.missing_glyph) add("MISSING_GLYPH");
  if (out.empty()) out = "\"OK\"";
  return out;
}

std::string hex_cp(std::uint32_t cp) {
  char buf[16];
  std::snprintf(buf, sizeof(buf), "U+%04X", cp);
  return buf;
}

void append_bbox_json(std::string& out, const BBox& box) {
  out += "{\"x\":";
  out += format_fixed3(box.valid ? box.x26 : 0);
  out += ",\"y\":";
  out += format_fixed3(box.valid ? box.y26 : 0);
  out += ",\"w\":";
  out += format_fixed3(box.valid ? box.w26 : 0);
  out += ",\"h\":";
  out += format_fixed3(box.valid ? box.h26 : 0);
  out += "}";
}
}  // namespace

std::uint64_t fnv1a64(std::string_view bytes) {
  std::uint64_t hash = 14695981039346656037ULL;
  for (unsigned char c : bytes) {
    hash ^= c;
    hash *= 1099511628211ULL;
  }
  return hash;
}

std::string format_fixed3(int value26) {
  const bool negative = value26 < 0;
  const std::int64_t abs_value = negative ? -static_cast<std::int64_t>(value26)
                                          : static_cast<std::int64_t>(value26);
  std::int64_t whole = abs_value / 64;
  std::int64_t frac = ((abs_value % 64) * 1000 + 32) / 64;
  if (frac >= 1000) { frac -= 1000; ++whole; }
  std::string out = negative && (whole != 0 || frac != 0) ? "-" : "";
  out += std::to_string(whole);
  out += ".";
  if (frac < 100) out += "0";
  if (frac < 10) out += "0";
  out += std::to_string(frac);
  return out;
}

std::string json_escape(std::string_view text) {
  std::string out;
  out.reserve(text.size() + 8);
  for (unsigned char c : text) {
    switch (c) {
      case '"': out += "\\\""; break;
      case '\\': out += "\\\\"; break;
      case '\b': out += "\\b"; break;
      case '\f': out += "\\f"; break;
      case '\n': out += "\\n"; break;
      case '\r': out += "\\r"; break;
      case '\t': out += "\\t"; break;
      default:
        if (c < 0x20) {
          char buf[8];
          std::snprintf(buf, sizeof(buf), "\\u%04x", c);
          out += buf;
        } else {
          out.push_back(static_cast<char>(c));
        }
    }
  }
  return out;
}

Report build_report(const CueDocument& document, FontFace& font,
                    const LayoutOptions& options) {
  Report report;
  report.cues.reserve(document.cues.size());
  for (const auto& cue : document.cues) {
    CueMeasurement measurement;
    measurement.cue = cue;
    measurement.decoded = decode_utf8(cue.text);
    const auto breaks = analyze_breaks(measurement.decoded.clusters);
    measurement.layout = layout_cue(measurement.decoded.clusters, breaks, font, options);
    report.lines_total += measurement.layout.lines.size();
    for (const auto& line : measurement.layout.lines)
      report.max_line_width26 = std::max(report.max_line_width26, line.width26);
    if (measurement.layout.ok()) ++report.cues_ok;
    count_overflow(measurement.layout, report);
    report.cues.push_back(std::move(measurement));
  }
  report.cues_total = report.cues.size();
  report.invalid_utf8_total = document.invalid_utf8;
  report.overflow_total = report.cues_total - report.cues_ok;
  compute_report_hash(report);
  return report;
}

std::string render_report_json_without_hash(const Report& report) {
  std::string out;
  out += "{\n";
  out += "  \"schema\":\"smx-measure-v1\",\n";
  out += "  \"cues_total\":" + std::to_string(report.cues_total) + ",\n";
  out += "  \"cues_ok\":" + std::to_string(report.cues_ok) + ",\n";
  out += "  \"overflow_total\":" + std::to_string(report.overflow_total) + ",\n";
  out += "  \"overflow\":{\"width\":" + std::to_string(report.overflow_width) +
         ",\"lines\":" + std::to_string(report.overflow_lines) +
         ",\"unbreakable\":" + std::to_string(report.overflow_unbreakable) +
         ",\"missing_glyph\":" + std::to_string(report.overflow_missing_glyph) + "},\n";
  out += "  \"lines_total\":" + std::to_string(report.lines_total) + ",\n";
  out += "  \"max_line_width_px\":" + format_fixed3(report.max_line_width26) + ",\n";
  out += "  \"invalid_utf8_total\":" + std::to_string(report.invalid_utf8_total) + ",\n";
  out += "  \"cues\":[\n";
  for (std::size_t ci = 0; ci < report.cues.size(); ++ci) {
    const auto& cue = report.cues[ci];
    out += "    {\"id\":\"" + json_escape(cue.cue.id) + "\",\"style\":\"" +
           json_escape(cue.cue.style) + "\",\"segment\":\"" +
           json_escape(cue.cue.segment) + "\",\"text\":\"" + json_escape(cue.cue.text) +
           "\",\"bbox\":";
    append_bbox_json(out, cue.layout.bbox);
    out += ",\"line_count\":" + std::to_string(cue.layout.lines.size());
    out += ",\"invalid_utf8\":" + std::to_string(cue.cue.invalid_utf8);
    out += ",\"overflow\":[" + overflow_names(cue.layout) + "]";
    out += ",\"missing_glyph_count\":" + std::to_string(cue.layout.missing_glyph_count);
    out += ",\"missing_glyphs\":[";
    for (std::size_t mi = 0; mi < cue.layout.missing_glyphs.size(); ++mi) {
      if (mi) out += ",";
      out += "\"" + hex_cp(cue.layout.missing_glyphs[mi]) + "\"";
    }
    out += "],\"lines\":[\n";
    for (std::size_t li = 0; li < cue.layout.lines.size(); ++li) {
      const auto& line = cue.layout.lines[li];
      out += "      {\"start\":" + std::to_string(line.start) +
             ",\"end\":" + std::to_string(line.end) +
             ",\"width_px\":" + format_fixed3(line.width26) + ",\"bbox\":";
      append_bbox_json(out, line.bbox);
      out += ",\"break_reason\":\"" + std::string(break_reason_name(line.reason)) + "\"}";
      if (li + 1 < cue.layout.lines.size()) out += ",";
      out += "\n";
    }
    out += "    ]}";
    if (ci + 1 < report.cues.size()) out += ",";
    out += "\n";
  }
  out += "  ]\n}\n";
  return out;
}

void compute_report_hash(Report& report) {
  const std::string canonical = render_report_json_without_hash(report);
  char buf[17];
  std::snprintf(buf, sizeof(buf), "%016llx",
                static_cast<unsigned long long>(fnv1a64(canonical)));
  report.report_hash = buf;
}

std::string render_report_json(const Report& report) {
  const std::string without = render_report_json_without_hash(report);
  const std::string marker = "  \"cues\":[\n";
  const auto at = without.find(marker);
  if (at == std::string::npos) return without;
  std::string out = without.substr(0, at);
  out += "  \"report_hash\":\"" + report.report_hash + "\",\n";
  out += without.substr(at);
  return out;
}

std::string render_report_tsv(const Report& report) {
  std::ostringstream out;
  out << "cue_id\tstyle\tsegment\tline\tstart\tend\twidth_px\tbbox_x\tbbox_y\tbbox_w\tbbox_h"
         "\tbreak_reason\toverflow\tinvalid_utf8\tmissing_glyph_count\tmissing_glyphs\n";
  for (const auto& cue : report.cues) {
    std::string overflow;
    auto add = [&](const char* name) {
      if (!overflow.empty()) overflow += ";";
      overflow += name;
    };
    if (cue.layout.width_overflow) add("WIDTH");
    if (cue.layout.lines_overflow) add("LINES");
    if (cue.layout.unbreakable) add("UNBREAKABLE");
    if (cue.layout.missing_glyph) add("MISSING_GLYPH");
    if (overflow.empty()) overflow = "OK";
    std::string missing;
    for (std::size_t i = 0; i < cue.layout.missing_glyphs.size(); ++i) {
      if (i) missing += ";";
      missing += hex_cp(cue.layout.missing_glyphs[i]);
    }
    for (std::size_t li = 0; li < cue.layout.lines.size(); ++li) {
      const auto& line = cue.layout.lines[li];
      out << cue.cue.id << '\t' << cue.cue.style << '\t' << cue.cue.segment << '\t' << li
          << '\t' << line.start << '\t' << line.end << '\t' << format_fixed3(line.width26)
          << '\t' << format_fixed3(line.bbox.valid ? line.bbox.x26 : 0) << '\t'
          << format_fixed3(line.bbox.valid ? line.bbox.y26 : 0) << '\t'
          << format_fixed3(line.bbox.valid ? line.bbox.w26 : 0) << '\t'
          << format_fixed3(line.bbox.valid ? line.bbox.h26 : 0) << '\t'
          << break_reason_name(line.reason) << '\t' << overflow << '\t'
          << cue.cue.invalid_utf8 << '\t' << cue.layout.missing_glyph_count << '\t'
          << missing << '\n';
    }
  }
  return out.str();
}

bool write_text_file(const std::string& path, const std::string& text, std::string& error) {
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) { error = "cannot open output file: " + path; return false; }
  out.write(text.data(), static_cast<std::streamsize>(text.size()));
  if (!out) { error = "cannot write output file: " + path; return false; }
  return true;
}
}  // namespace smx
