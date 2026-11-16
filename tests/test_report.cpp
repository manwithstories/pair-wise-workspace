#include "gop_analyzer.h"
#include "h264_test_helpers.h"
#include "report_writer.h"
#include "slice_parser.h"
#include "sps_parser.h"
#include "test_framework.h"

#include <sstream>
#include <string>

using namespace h264;

namespace {
AnalysisResult buildSample() {
  auto sps_nal = h264test::makeSps1080p(false);
  SpsInfo sps;
  SpsParser::parse(sps_nal, sps);
  GopAnalyzer a;
  a.processNal(sps_nal);
  a.processNal(h264test::makePps());

  auto mk = [&](uint8_t h, uint8_t st, uint32_t fn, uint64_t off) {
    BitWriter w;
    w.writeUE(0);
    w.writeUE(st);
    w.writeUE(0);
    w.writeBits(fn, sps.log2_max_frame_num_minus4 + 4);
    if ((h & 0x1Fu) == NAL_IDR_SLICE)
      w.writeUE(0);
    w.writeBits(0, sps.log2_max_pic_order_cnt_lsb_minus4 + 4);
    w.writeRbspTrailingBits();
    auto nal = h264test::wrapNal(h, w.bytes());
    nal.start_offset = off;
    return nal;
  };
  a.processNal(mk(0x65, SLICE_I, 0, 100));
  a.processNal(mk(0x41, SLICE_P, 1, 120));
  a.processNal(mk(0x01, SLICE_B, 2, 140));
  return a.finish();
}
} // namespace

TEST_CASE(report_json_contains_stable_named_fields) {
  auto r = buildSample();
  std::ostringstream out;
  ReportWriter::write(out, r, ReportFormat::JSON, false, "sample.264");
  const std::string j = out.str();

  // 关键稳定字段名必须存在
  for (const char *key :
       {"\"profile_idc\"", "\"level_idc\"", "\"coded_width\"",
        "\"coded_height\"", "\"nal_count\"", "\"frame_count\"",
        "\"slice_count\"", "\"gop_count\"", "\"i_count\"", "\"p_count\"",
        "\"b_count\"", "\"start_offset\"", "\"field_pair_count\"",
        "\"exp_golomb_calls\"", "\"warning_count\""}) {
    CHECK(j.find(key) != std::string::npos);
  }
  CHECK(j.find("\"profile_idc\": 100") != std::string::npos);
  CHECK(j.find("\"level_idc\": 40") != std::string::npos);
  CHECK(j.find("\"coded_width\": 1920") != std::string::npos);
  CHECK(j.find("\"coded_height\": 1080") != std::string::npos);
  CHECK(j.find("\"gop_count\": 1") != std::string::npos);
}

TEST_CASE(report_table_contains_summary) {
  auto r = buildSample();
  std::ostringstream out;
  ReportWriter::write(out, r, ReportFormat::TABLE, true, "sample.264");
  const std::string t = out.str();
  CHECK(t.find("h264inspect report") != std::string::npos);
  CHECK(t.find("[strict]") != std::string::npos);
  CHECK(t.find("start_offset") != std::string::npos);
  CHECK(t.find("exp_golomb") != std::string::npos);
}

TEST_CASE(report_json_escape) {
  CHECK_EQ(ReportWriter::jsonEscape("a\"b\\c"), "a\\\"b\\\\c");
  CHECK_EQ(ReportWriter::jsonEscape("x\ny"), "x\\ny");
}

TEST_CASE(report_golomb_counts_hundreds_level) {
  auto r = buildSample();
  // 每个 SPS/PPS/slice 都含若干 ue/se，累计应为正且 max_code_bits 合理
  CHECK(r.golomb.success > 0);
  CHECK(r.golomb.max_code_bits >= 1);
}
