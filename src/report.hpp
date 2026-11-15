#pragma once
#include <cstddef>
#include <cstdint>
#include <string>
#include <string_view>
#include <vector>
#include "cue_parse.hpp"
#include "font_face.hpp"
#include "layout.hpp"

namespace smx {
struct CueMeasurement {
  Cue cue;
  Utf8DecodeResult decoded;
  LayoutResult layout;
};

struct Report {
  std::vector<CueMeasurement> cues;
  std::size_t cues_total = 0;
  std::size_t cues_ok = 0;
  std::size_t overflow_total = 0;
  std::size_t overflow_width = 0;
  std::size_t overflow_lines = 0;
  std::size_t overflow_unbreakable = 0;
  std::size_t overflow_missing_glyph = 0;
  std::size_t lines_total = 0;
  std::size_t invalid_utf8_total = 0;
  int max_line_width26 = 0;
  std::string report_hash;
};

Report build_report(const CueDocument& document, FontFace& font,
                    const LayoutOptions& options);
void compute_report_hash(Report& report);
std::string render_report_json(const Report& report);
std::string render_report_tsv(const Report& report);
std::string render_report_json_without_hash(const Report& report);
bool write_text_file(const std::string& path, const std::string& text, std::string& error);
std::uint64_t fnv1a64(std::string_view bytes);
std::string format_fixed3(int value26);
std::string json_escape(std::string_view text);
}  // namespace smx
