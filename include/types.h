#pragma once

#include <cstdint>
#include <string>
#include <utility>
#include <vector>

namespace sublay {

// ---- 26.6 fixed point ----
using fixed_t = int32_t;
inline fixed_t ft_from_int(int v) { return static_cast<fixed_t>(v) << 6; }
inline int ft_to_int(fixed_t v) { return (v + 32) >> 6; }
inline fixed_t ft_mul(fixed_t a, fixed_t b) {
  return static_cast<fixed_t>((static_cast<int64_t>(a) * b + 32) >> 6);
}
inline fixed_t ft_div(fixed_t a, fixed_t b) {
  return static_cast<fixed_t>((static_cast<int64_t>(a) << 6) / b);
}

struct Style {
  bool bold = false;
  bool italic = false;
  bool underline = false;
  bool operator==(const Style& o) const {
    return bold == o.bold && italic == o.italic && underline == o.underline;
  }
  bool operator!=(const Style& o) const { return !(*this == o); }
};

// A segment of text ready for shaping: uniform direction, style and font.
struct TextRun {
  size_t byte_off = 0;   // offset into the cue's plain text (bytes)
  std::string text;
  std::string lang;
  int font_index = 0;
  Style style;
  bool rtl = false;
};

struct GlyphBox {
  uint32_t glyph = 0;
  size_t cluster = 0;    // byte offset in plain text
  fixed_t advance = 0;
  fixed_t bearing_x = 0;
  fixed_t bearing_y = 0;
  fixed_t width = 0;
  fixed_t height = 0;
  int font_index = 0;
};

struct LineBox {
  fixed_t x_min = 0, y_min = 0, x_max = 0, y_max = 0;
  fixed_t baseline = 0;
  fixed_t width = 0;
  size_t start_byte = 0, end_byte = 0;
};

struct CueMetrics {
  std::string cue_id;
  int line_count = 0;
  LineBox bbox;
  std::vector<size_t> breaks;   // logical byte offsets, ascending
  bool overflow = false;
  fixed_t overflow_px = 0;
  bool missing_glyph = false;
  int glyph_count = 0;
  std::vector<LineBox> lines;
  std::vector<std::string> rules;
};

struct Cue {
  std::string id, lang, speaker, text;
  int64_t start_ms = 0, end_ms = 0;
};

struct ParseIssue {
  int line = 0, col = 0;
  std::string message;
};

struct ParseResult {
  std::vector<Cue> cues;
  std::vector<ParseIssue> issues;   // bad cues skipped and counted here
};

struct LayoutParams {
  int size_px = 48;
  int width = 1920;
  int safe = 160;
  int max_lines = 2;
};

// ---- seg.cpp ----
struct UChar {
  uint32_t cp = 0;
  size_t byte_off = 0;
};
// Strict UTF-8 decode. Returns false and sets err_col on invalid input.
bool utf8_decode(const std::string& s, std::vector<UChar>& out, size_t& err_col);

bool is_combining(uint32_t cp);
bool is_variation_selector(uint32_t cp);
bool is_zwj(uint32_t cp);
bool is_rtl_cp(uint32_t cp);
bool is_cjk_cp(uint32_t cp);
bool is_space_cp(uint32_t cp);
bool kinsoku_no_line_start(uint32_t cp);  // must not begin a line
bool kinsoku_no_line_end(uint32_t cp);    // must not end a line
bool is_url_char(uint32_t cp);            // unbreakable URL body char

// Grapheme-ish cluster: base plus combining marks / VS / ZWJ sequences.
struct Cluster {
  size_t byte_off = 0;
  size_t byte_len = 0;
  uint32_t first_cp = 0;
  bool rtl = false;
  bool zero_width = false;  // pure VS/ZWJ/control cluster
};
std::vector<Cluster> make_clusters(const std::vector<UChar>& cps, size_t text_len);

// ---- parse.cpp ----
ParseResult parse_subtxt(const std::string& path);
// Strips inline tags; returns plain text plus style spans [start,end) in bytes.
struct StyledText {
  std::string text;
  std::vector<std::pair<size_t, size_t>> bold, italic, underline;
  std::vector<size_t> forced_breaks;  // byte offsets of '\n' markers
};
bool strip_markup(const std::string& raw, StyledText& out, std::string& err);

// ---- font.cpp ----
class FontChain {
public:
  FontChain();
  ~FontChain();
  FontChain(const FontChain&) = delete;
  FontChain& operator=(const FontChain&) = delete;
  // paths[0] is primary, rest are fallbacks. size_px <= 0 is rejected.
  bool load(const std::vector<std::string>& paths, int size_px, std::string& err);
  // Pick a font covering cp; returns font index or -1 when exhausted.
  int pick_font(uint32_t cp) const;
  // Shape one run with fonts[run.font_index]; appends glyphs.
  void shape_run(const TextRun& run, std::vector<GlyphBox>& out);
  fixed_t ascender() const;
  fixed_t descender() const;
  fixed_t line_height() const;
  fixed_t missing_advance() const;
  int size_px() const;

private:
  struct Impl;
  Impl* p_;
};

// ---- break.cpp ----
struct ClusterInfo {
  size_t byte_off = 0;
  size_t byte_len = 0;
  fixed_t width = 0;          // ink/advance width used for line fitting
  bool break_after = false;   // candidate break point after this cluster
  bool word_cut = false;      // break here is a word-internal hard cut
  bool no_line_start = false; // cluster must not start a line
  bool no_line_end = false;   // cluster must not end a line
  bool trailing_space = false;// collapsible at line end
};
struct BreakResult {
  std::vector<size_t> breaks;        // logical byte offsets, ascending
  std::vector<size_t> line_start, line_end;  // byte ranges per line
  std::vector<fixed_t> line_widths;
  bool overflow = false;
  fixed_t overflow_px = 0;
  std::vector<std::string> rules;
};
BreakResult layout_lines(const std::vector<ClusterInfo>& clusters,
                         const std::vector<size_t>& forced_breaks,
                         fixed_t safe_width, int max_lines);

// ---- measure.cpp ----
struct MeasureStats {
  int cues = 0;
  long glyphs = 0;
  long breaks = 0;
  int overflow_count = 0;
  int missing_glyph = 0;
};
// Lay out one cue; the layouter is shared by measure and export paths.
CueMetrics layout_cue(const Cue& cue, FontChain& fonts, const LayoutParams& params);
struct MeasureOutput {
  std::vector<CueMetrics> cues;
  MeasureStats stats;
};
MeasureOutput measure_all(const std::vector<Cue>& cues, FontChain& fonts,
                          const LayoutParams& params, int jobs);

// ---- export.cpp ----
std::string cue_metrics_json(const CueMetrics& m, int indent);
std::string summary_json(const MeasureOutput& out, const LayoutParams& params,
                         const std::vector<std::string>& font_paths);
bool write_file(const std::string& path, const std::string& data, std::string& err);
// A cue record read back from an on-disk summary (export consumes, never
// recomputes, the layouter's results).
struct CueRecord {
  std::string cue_id;
  long line_count = 0;
  std::vector<size_t> breaks;
  bool overflow = false;
  long overflow_px = 0;
};
// JSONL export: one record per cue, breaks reused from the summary.
std::string jsonl_from_records(const std::vector<CueRecord>& recs);
// Minimal JSON readers for verify/export of on-disk results.
bool json_read_cues(const std::string& json, std::vector<CueRecord>& out);
bool json_get_int(const std::string& json, const std::string& key, long& out);

}  // namespace sublay
