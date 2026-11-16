#include "report_writer.h"

#include <cstdio>
#include <iomanip>
#include <sstream>

namespace h264 {

std::string ReportWriter::jsonEscape(const std::string &s) {
  std::string out;
  out.reserve(s.size() + 2);
  for (char c : s) {
    switch (c) {
    case '"': out += "\\\""; break;
    case '\\': out += "\\\\"; break;
    case '\n': out += "\\n"; break;
    case '\r': out += "\\r"; break;
    case '\t': out += "\\t"; break;
    default:
      if (static_cast<unsigned char>(c) < 0x20) {
        char buf[8];
        std::snprintf(buf, sizeof(buf), "\\u%04x",
                      static_cast<unsigned char>(c));
        out += buf;
      } else {
        out += c;
      }
    }
  }
  return out;
}

namespace {

const char *typeChar(uint8_t t) { return SliceParser::typeName(t); }

void writeJson(std::ostream &o, const AnalysisResult &r, bool strict,
               const std::string &source) {
  o << "{\n";
  o << "  \"source\": \"" << ReportWriter::jsonEscape(source) << "\",\n";
  o << "  \"mode\": \"" << (strict ? "strict" : "tolerant") << "\",\n";

  // 汇总计数（字段名固定，顺序稳定）
  o << "  \"nal_count\": " << r.nal_count << ",\n";
  o << "  \"vcl_nal_count\": " << r.vcl_nal_count << ",\n";
  o << "  \"truncated_nal_count\": " << r.truncated_nal_count << ",\n";
  o << "  \"frame_count\": " << r.pictures.size() << ",\n";
  o << "  \"slice_count\": " << r.slice_count << ",\n";
  o << "  \"gop_count\": " << r.gops.size() << ",\n";
  o << "  \"warning_count\": " << r.warnings.size() << ",\n";
  o << "  \"parse_failure_count\": " << r.parse_failure_count << ",\n";
  o << "  \"field_pair_count\": " << r.field_pairs.size() << ",\n";
  o << "  \"field_pair_complete\": " << r.field_pair_complete << ",\n";
  o << "  \"field_pair_incomplete\": " << r.field_pair_incomplete << ",\n";
  o << "  \"min_slices_per_picture\": " << r.min_slices_per_pic << ",\n";
  o << "  \"max_slices_per_picture\": " << r.max_slices_per_pic << ",\n";

  uint32_t i = 0, p = 0, b = 0;
  for (const auto &pic : r.pictures) {
    if (pic.is_i) ++i;
    else if (pic.is_p) ++p;
    else if (pic.is_b) ++b;
  }
  o << "  \"i_count\": " << i << ",\n";
  o << "  \"p_count\": " << p << ",\n";
  o << "  \"b_count\": " << b << ",\n";

  o << "  \"exp_golomb_calls\": " << r.golomb.success << ",\n";
  o << "  \"exp_golomb_failures\": " << r.golomb.failure << ",\n";
  o << "  \"exp_golomb_max_code_bits\": " << r.golomb.max_code_bits
    << ",\n";

  // SPS（取首个，夹具只生成一个）
  if (!r.sps.empty()) {
    const SpsInfo &s = r.sps.front();
    o << "  \"profile_idc\": " << static_cast<unsigned>(s.profile_idc)
      << ",\n";
    o << "  \"level_idc\": " << static_cast<unsigned>(s.level_idc) << ",\n";
    o << "  \"coded_width\": " << s.coded_width << ",\n";
    o << "  \"coded_height\": " << s.coded_height << ",\n";
    o << "  \"max_num_ref_frames\": " << s.max_num_ref_frames << ",\n";
    o << "  \"frame_mbs_only_flag\": " << (s.frame_mbs_only_flag ? 1 : 0)
      << ",\n";
    o << "  \"mb_adaptive_frame_field_flag\": "
      << (s.mb_adaptive_frame_field_flag ? 1 : 0) << ",\n";
    o << "  \"fps\": " << std::fixed << std::setprecision(3) << s.fps
      << ",\n";
  }

  // GOP 明细
  o << "  \"gops\": [\n";
  for (size_t gi = 0; gi < r.gops.size(); ++gi) {
    const GopInfo &g = r.gops[gi];
    o << "    {\"gop_id\": " << g.gop_id
      << ", \"start_offset\": " << g.start_offset
      << ", \"start_slice_type\": \"" << typeChar(g.start_slice_type) << "\""
      << ", \"starts_with_idr\": " << (g.starts_with_idr ? 1 : 0)
      << ", \"picture_count\": " << g.picture_count
      << ", \"i_count\": " << g.i_count
      << ", \"p_count\": " << g.p_count
      << ", \"b_count\": " << g.b_count << "}"
      << (gi + 1 < r.gops.size() ? "," : "") << "\n";
  }
  o << "  ],\n";

  // 场对明细
  o << "  \"field_pairs\": [\n";
  for (size_t fi = 0; fi < r.field_pairs.size(); ++fi) {
    const FieldPairInfo &fp = r.field_pairs[fi];
    o << "    {\"frame_num\": " << fp.frame_num
      << ", \"top_present\": " << (fp.top_present ? 1 : 0)
      << ", \"bottom_present\": " << (fp.bottom_present ? 1 : 0)
      << ", \"complete\": " << (fp.complete ? 1 : 0) << "}"
      << (fi + 1 < r.field_pairs.size() ? "," : "") << "\n";
  }
  o << "  ],\n";

  // 警告明细
  o << "  \"warnings\": [\n";
  for (size_t wi = 0; wi < r.warnings.size(); ++wi) {
    const AnalyzerWarning &w = r.warnings[wi];
    o << "    {\"nal_offset\": " << w.nal_offset << ", \"reason\": \""
      << ReportWriter::jsonEscape(w.reason) << "\"}"
      << (wi + 1 < r.warnings.size() ? "," : "") << "\n";
  }
  o << "  ]\n";
  o << "}\n";
}

void writeTable(std::ostream &o, const AnalysisResult &r, bool strict,
                const std::string &source) {
  o << "h264inspect report — " << source << " ["
    << (strict ? "strict" : "tolerant") << "]\n";
  o << "========================================"
       "====================================\n";
  if (!r.sps.empty()) {
    const SpsInfo &s = r.sps.front();
    o << "profile_idc=" << static_cast<unsigned>(s.profile_idc)
      << "  level_idc=" << static_cast<unsigned>(s.level_idc)
      << "  coded=" << s.coded_width << "x" << s.coded_height
      << "  frame_mbs_only=" << (s.frame_mbs_only_flag ? 1 : 0)
      << "  mbaff=" << (s.mb_adaptive_frame_field_flag ? 1 : 0) << "\n";
  }
  o << "nal_count=" << r.nal_count
    << "  frame_count=" << r.pictures.size()
    << "  slice_count=" << r.slice_count
    << "  gop_count=" << r.gops.size() << "\n";
  uint32_t ic = 0, pc = 0, bc = 0;
  for (const auto &pic : r.pictures) {
    if (pic.is_i) ++ic;
    else if (pic.is_p) ++pc;
    else if (pic.is_b) ++bc;
  }
  o << "I=" << ic << " P=" << pc << " B=" << bc
    << "  field_pairs=" << r.field_pairs.size()
    << " (complete=" << r.field_pair_complete
    << ", incomplete=" << r.field_pair_incomplete << ")\n";
  o << "slices/picture min=" << r.min_slices_per_pic
    << " max=" << r.max_slices_per_pic << "\n";
  o << "exp_golomb calls=" << r.golomb.success
    << " failures=" << r.golomb.failure
    << " max_code_bits=" << r.golomb.max_code_bits << "\n";
  o << "warnings=" << r.warnings.size()
    << " parse_failures=" << r.parse_failure_count
    << " truncated=" << r.truncated_nal_count << "\n\n";

  o << "gop  start_offset  type  idr  pictures  I  P  B\n";
  for (const GopInfo &g : r.gops) {
    o << std::setw(3) << g.gop_id << "  " << std::setw(12)
      << g.start_offset << "  " << std::setw(4)
      << typeChar(g.start_slice_type) << "  " << std::setw(3)
      << (g.starts_with_idr ? "yes" : "no") << "  " << std::setw(8)
      << g.picture_count << "  " << g.i_count << "  " << g.p_count << "  "
      << g.b_count << "\n";
  }
  if (!r.warnings.empty()) {
    o << "\nwarnings:\n";
    for (const auto &w : r.warnings)
      o << "  @" << w.nal_offset << ": " << w.reason << "\n";
  }
}

} // namespace

void ReportWriter::write(std::ostream &out, const AnalysisResult &r,
                         ReportFormat fmt, bool mode_strict,
                         const std::string &source_name) {
  if (fmt == ReportFormat::JSON)
    writeJson(out, r, mode_strict, source_name);
  else
    writeTable(out, r, mode_strict, source_name);
}

} // namespace h264
