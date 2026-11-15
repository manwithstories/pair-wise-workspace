#include "selftest.hpp"

#include <algorithm>
#include <cctype>
#include <filesystem>
#include <fstream>
#include <sstream>
#include <string>
#include <vector>

#include "break_opp.hpp"
#include "cue_parse.hpp"
#include "font_face.hpp"
#include "layout.hpp"
#include "report.hpp"
#include "ttf_synth.hpp"
#include "utf8.hpp"

namespace smx {
namespace {
class Checker {
 public:
  void check(bool condition, const std::string& message) {
    ++checks_;
    if (!condition && ok_) {
      ok_ = false;
      failure_ = message;
    }
  }
  bool ok() const { return ok_; }
  const std::string& failure() const { return failure_; }
  std::size_t checks() const { return checks_; }

 private:
  bool ok_ = true;
  std::string failure_;
  std::size_t checks_ = 0;
};

struct TempFont {
  std::filesystem::path path;
  ~TempFont() {
    std::error_code ec;
    std::filesystem::remove(path, ec);
  }
};

bool write_bytes(const std::filesystem::path& path, const std::vector<std::uint8_t>& bytes) {
  std::ofstream out(path, std::ios::binary | std::ios::trunc);
  if (!out) return false;
  out.write(reinterpret_cast<const char*>(bytes.data()),
            static_cast<std::streamsize>(bytes.size()));
  return static_cast<bool>(out);
}

bool has_break(const BreakResult& breaks, std::size_t pos, BreakReason reason) {
  return std::any_of(breaks.breaks.begin(), breaks.breaks.end(), [&](const BreakPoint& b) {
    return b.pos == pos && b.reason == reason;
  });
}

bool has_any_break(const BreakResult& breaks, std::size_t pos) {
  return std::any_of(breaks.breaks.begin(), breaks.breaks.end(),
                     [&](const BreakPoint& b) { return b.pos == pos; });
}

LayoutResult layout_text(const std::string& utf8, FontFace& font, int max_width_px,
                         int max_lines, LayoutStrategy strategy = LayoutStrategy::Dp,
                         TruncatePolicy truncate = TruncatePolicy::Keep) {
  const auto decoded = decode_utf8(utf8);
  const auto breaks = analyze_breaks(decoded.clusters);
  LayoutOptions options;
  options.max_width26 = max_width_px * 64;
  options.max_lines = max_lines;
  options.strategy = strategy;
  options.truncate = truncate;
  return layout_cue(decoded.clusters, breaks, font, options);
}

bool valid_hash(const std::string& hash) {
  return hash.size() == 16 &&
         std::all_of(hash.begin(), hash.end(), [](unsigned char c) { return std::isxdigit(c); });
}
}  // namespace

SelfTestResult run_selftest() {
  Checker c;
  const auto first = synth_font(7);
  const auto second = synth_font(7);
  const auto other = synth_font(8);
  c.check(first.ok && second.ok && other.ok, "synthetic font generation failed");
  c.check(first.bytes == second.bytes, "same seed must produce identical font bytes");
  c.check(first.bytes != other.bytes, "different seed must produce different font bytes");

  TempFont temp;
  temp.path = std::filesystem::temp_directory_path() / "smx_selftest_font.ttf";
  c.check(write_bytes(temp.path, first.bytes), "cannot write temp font");
  FontFace font;
  std::string error;
  c.check(font.open(temp.path.string(), {}, 32, error), "FreeType cannot open synth font: " + error);
  c.check(font.line_height26() > 0, "font line height must be positive");
  c.check(font.ascender26() > 0 && font.descender26() < 0, "font vertical metrics invalid");

  const auto invalid = decode_utf8(std::string("A\xff", 2));
  c.check(invalid.invalid_sequences == 1, "invalid UTF-8 must be counted");
  c.check(invalid.clusters.size() == 2 && invalid.clusters[1].first_cp == 0xfffd,
          "invalid UTF-8 must become U+FFFD");
  const auto truncated_utf8 = decode_utf8("\xe4\xb8");
  c.check(truncated_utf8.invalid_sequences == 1,
          "truncated multibyte sequence must count as one replacement");
  const auto combined = decode_utf8("e\xcc\x81");
  c.check(combined.clusters.size() == 1 && combined.clusters[0].cps.size() == 2,
          "combining mark must join the preceding cluster");
  const auto zero = decode_utf8("A\xe2\x80\x8b" "B");
  c.check(zero.clusters.size() == 3, "ZWSP must remain a break-participating cluster");
  ClusterMetric e_metric, combined_metric;
  c.check(font.measure_cluster(combined.clusters[0], combined_metric, nullptr),
          "combined cluster measurement failed");
  const auto e_only = decode_utf8("e");
  c.check(font.measure_cluster(e_only.clusters[0], e_metric, nullptr),
          "base cluster measurement failed");
  c.check(e_metric.advance26 == combined_metric.advance26,
          "combining mark must not add advance");

  const auto breaks = analyze_breaks(zero.clusters);
  c.check(has_break(breaks, 2, BreakReason::Zwsp), "ZWSP must create a break opportunity");
  const auto nbsp = decode_utf8("A\xc2\xa0" "B");
  c.check(!has_any_break(analyze_breaks(nbsp.clusters), 2), "NBSP must not break");
  const auto hyphen = decode_utf8("A-B");
  c.check(has_break(analyze_breaks(hyphen.clusters), 2, BreakReason::Hyphen),
          "ASCII hyphen must break");
  const auto nb_hyphen = decode_utf8("A\xe2\x80\x91" "B");
  c.check(!has_any_break(analyze_breaks(nb_hyphen.clusters), 2),
          "non-breaking hyphen must not break");
  const auto cjk = decode_utf8("中文文字");
  const auto cjk_breaks = analyze_breaks(cjk.clusters);
  c.check(cjk_breaks.breaks.size() >= 3, "CJK must offer per-cluster breaks");
  const auto forbidden = decode_utf8("中「文」。好");
  const auto forbidden_breaks = analyze_breaks(forbidden.clusters);
  c.check(!has_any_break(forbidden_breaks, 2), "line-end opening punctuation must roll back");
  c.check(!has_any_break(forbidden_breaks, 3), "line-start closing punctuation must roll back");
  const auto hard = decode_utf8("A\nB");
  const auto hard_breaks = analyze_breaks(hard.clusters);
  c.check(has_break(hard_breaks, 1, BreakReason::Hard), "hard newline must be a forced break");
  c.check(hard_breaks.hard_after.size() == 3 && hard_breaks.hard_after[1],
          "hard newline priority flag missing");

  const auto missing_text = decode_utf8("\xf0\x9f\x98\x80");
  ClusterMetric missing_metric;
  std::vector<std::uint32_t> missing;
  font.measure_cluster(missing_text.clusters[0], missing_metric, &missing);
  c.check(!missing.empty() && missing[0] == 0x1f600, "missing glyph must be recorded");

  const auto av = decode_utf8("AV");
  ClusterMetric a_metric, v_metric;
  font.measure_cluster(av.clusters[0], a_metric, nullptr);
  font.measure_cluster(av.clusters[1], v_metric, nullptr);
  const int kern = font.kerning_between(a_metric, v_metric);
  c.check(kern != 0, "synthetic font kerning was not applied");
  LayoutOptions av_options;
  av_options.max_width26 = 10000;
  av_options.max_lines = 1;
  av_options.strategy = LayoutStrategy::Dp;
  const auto av_layout = layout_cue(av.clusters, analyze_breaks(av.clusters), font, av_options);
  c.check(!av_layout.lines.empty() &&
              av_layout.lines[0].width26 == a_metric.advance26 + kern + v_metric.advance26,
          "kerning must apply only between clusters");

  const LayoutResult greed = layout_text("中文文字测试", font, 128, 0, LayoutStrategy::Greedy);
  const LayoutResult dp = layout_text("中文文字测试", font, 128, 0, LayoutStrategy::Dp);
  c.check(greed.lines.size() == 2 && dp.lines.size() == 2,
          "greedy and DP should wrap six CJK glyphs into two lines");
  c.check(!dp.lines.empty() && dp.lines.front().bbox.valid && dp.lines.front().bbox.h26 > 0,
          "line bounding box must include glyph ink");
  c.check(dp.bbox.valid, "cue union bounding box must be valid");

  const LayoutResult lines_overflow = layout_text("中文文字测试", font, 128, 1);
  c.check(lines_overflow.lines_overflow && lines_overflow.lines.size() > 1,
          "line cap must classify LINES while keep policy preserves lines");
  const LayoutResult truncated =
      layout_text("中文文字测试", font, 128, 1, LayoutStrategy::Dp, TruncatePolicy::Ellipsis);
  c.check(truncated.lines.size() == 1, "ellipsis truncation must honor max_lines");
  c.check(!truncated.lines.empty() && truncated.lines[0].reason == BreakReason::Ellipsis &&
              truncated.lines[0].width26 <= 128 * 64,
          "ellipsis width must be included in the fit decision");

  const LayoutResult unbreakable =
      layout_text("Supercalifragilisticexpialidocious", font, 40, 0);
  c.check(unbreakable.unbreakable, "unbreakable run must be reported");
  const LayoutResult width_overflow = layout_text("中", font, 8, 0);
  c.check(width_overflow.width_overflow, "overwide glyph must classify WIDTH");
  c.check(width_overflow.unbreakable && !width_overflow.lines.empty() &&
              width_overflow.lines[0].reason == BreakReason::Forced,
          "overwide line must carry a forced break reason");
  const LayoutResult missing_overflow = layout_text("\xf0\x9f\x98\x80", font, 100, 0);
  c.check(missing_overflow.missing_glyph && missing_overflow.missing_glyph_count == 1,
          "missing glyph must classify MISSING_GLYPH and count occurrences");

  const std::string cue_source =
      "\xef\xbb\xbf# comment\r\nSTYLE default family=synth\r\n[style dark weight=bold]\r\n"
      "[segment intro]\r\n[cue c1 style=dark]\r\nA\\u200bB\r\n\r\n[cue c2]\r\nC\r\n";
  const CueDocument doc = parse_cues(cue_source);
  c.check(doc.errors.empty() && doc.cues.size() == 2, "BOM/CRLF/style/segment cue parsing failed");
  c.check(doc.cues.size() == 2 && doc.cues[0].id == "c1" && doc.cues[0].style == "dark" &&
              doc.cues[0].segment == "intro",
          "cue attributes were not retained");
  c.check(doc.cues.size() == 2 && doc.cues[0].text == "A\xe2\x80\x8b" "B",
          "unicode escape in cue text was not decoded");
  const CueDocument invalid_cue = parse_cues("[cue x]\nA\\xffB\n");
  c.check(invalid_cue.invalid_utf8 == 1 && invalid_cue.cues.size() == 1 &&
              decode_utf8(invalid_cue.cues[0].text).invalid_sequences == 0,
          "cue parser must replace and count invalid UTF-8");
  const CueDocument bad = parse_cues("[cue x]\n\n");
  c.check(!bad.errors.empty() && bad.errors[0].line == 2,
          "parse errors must carry their input line number");
  const CueDocument duplicate = parse_cues("[cue x]\nA\n[cue x]\nB\n");
  c.check(!duplicate.errors.empty() && duplicate.errors[0].line == 3,
          "duplicate cue id must report the conflicting line");

  Report report1 = build_report(doc, font, av_options);
  Report report2 = build_report(doc, font, av_options);
  c.check(report1.report_hash == report2.report_hash && valid_hash(report1.report_hash),
          "report hashes must be stable and 16 hex characters");
  c.check(report1.cues_total == 2 && report1.lines_total >= 2,
          "report summary counts are inconsistent");
  c.check(report1.invalid_utf8_total == 0,
          "valid cue report must not invent invalid UTF-8 counts");
  const std::string json = render_report_json(report1);
  c.check(json.find("\"report_hash\":\"") != std::string::npos &&
              json.find("\"overflow\":{\"width\":") != std::string::npos,
          "report JSON is missing required fields");
  c.check(render_report_tsv(report1).rfind("cue_id\t", 0) == 0,
          "TSV report header is missing");

  FontFace missing_font;
  std::string missing_error;
  c.check(!missing_font.open("/definitely/not/a/font.ttf", {}, 32, missing_error) &&
              !missing_error.empty(),
          "missing font must fail cleanly");

  SelfTestResult result;
  result.ok = c.ok();
  result.message = c.ok() ? std::to_string(c.checks()) + " checks passed" : c.failure();
  return result;
}
}  // namespace smx
