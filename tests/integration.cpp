#include "bit_reader.h"
#include "exp_golomb.h"
#include "fixture_writer.h"
#include "gop_analyzer.h"
#include "nal_scanner.h"
#include "report_writer.h"
#include "sps_parser.h"
#include "test_framework.h"

#include <fstream>
#include <sstream>
#include <string>

using namespace h264;

namespace {

// 端到端：缓冲 -> 扫描 -> 分析
AnalysisResult analyzeBytes(const std::vector<uint8_t> &d,
                            bool strict = false) {
  auto nals = NalScanner::scanAll(d.data(), d.size());
  GopAnalyzer a(strict);
  for (const auto &n : nals)
    a.processNal(n);
  return a.finish();
}

} // namespace

TEST_CASE(integration_end_to_end_acceptance_counts) {
  FixtureParams p;
  p.frames = 24;
  p.slices_per_frame = 3;
  auto data = generateFixture(p);
  auto r = analyzeBytes(data);

  // 从结构向量派生扁平计数（与 ReportWriter 的口径一致）
  uint32_t i = 0, pc = 0, b = 0;
  for (const auto &pic : r.pictures) {
    if (pic.is_i) ++i;
    else if (pic.is_p) ++pc;
    else if (pic.is_b) ++b;
  }
  CHECK_EQ(r.nal_count, 78u);
  CHECK_EQ(r.pictures.size(), 24u);
  CHECK_EQ(r.slice_count, 72u);
  CHECK_EQ(r.gops.size(), 3u);
  CHECK_EQ(i, 3u);
  CHECK_EQ(pc + b, 21u);
  CHECK_EQ(r.warnings.size(), 0u);
  CHECK_EQ(r.truncated_nal_count, 1u); // 末尾异常截断
  CHECK(!r.sps.empty());
  CHECK_EQ(r.sps[0].profile_idc, 100);
  CHECK_EQ(r.sps[0].level_idc, 40);
  CHECK_EQ(r.sps[0].coded_width, 1920u);
  CHECK_EQ(r.sps[0].coded_height, 1080u);
  for (const auto &g : r.gops) {
    CHECK(g.start_offset < r.nal_count * 100000u); // 非负且合理
    CHECK_EQ(g.i_count + g.p_count + g.b_count, g.picture_count);
  }
}

TEST_CASE(integration_interlaced_field_pairs) {
  FixtureParams p;
  p.frames = 24;
  p.slices_per_frame = 3;
  p.interlaced = true;
  auto r = analyzeBytes(generateFixture(p));
  CHECK_EQ(r.pictures.size(), 24u);
  CHECK(r.field_pairs.size() > 0);
  CHECK_EQ(r.field_pairs.size(), 24u);
  CHECK_EQ(r.field_pair_complete, 24u);
  CHECK_EQ(r.field_pair_incomplete, 0u);
  // 每图 = 顶场 3 片 + 底场 3 片 = 6 片
  CHECK_EQ(r.min_slices_per_pic, 6u);
  CHECK_EQ(r.max_slices_per_pic, 6u);
}

TEST_CASE(integration_cross_byte_golomb_consistency) {
  // 构造跨越字节边界的 ue/se 序列，验证 BitReader+ExpGolomb 协同
  std::vector<uint8_t> stream;
  // 手工拼接多个 ue 码字，刻意制造非字节对齐后跨字节读取
  const uint8_t bits[] = {
      0b10100100, 0b10001000, 0b11100101, 0b00100110, 0b10000000};
  stream.assign(bits, bits + sizeof(bits));
  BitReader br(stream.data(), stream.size());
  ExpGolomb eg(br);
  uint32_t v = 0;
  uint32_t decoded = 0;
  while (br.bitsLeft() > 0 && eg.readUE(v))
    ++decoded;
  CHECK(decoded > 0);
  // 成功解码的码字数必须与统计一致
  CHECK_EQ(eg.stats().success, decoded);
  CHECK(eg.stats().success + eg.stats().failure >= decoded);
}

TEST_CASE(integration_error_recovery_continues_after_bad_slice) {
  FixtureParams p;
  p.frames = 8;
  p.slices_per_frame = 1;
  p.append_truncated = false; // 先拿干净流
  auto data = generateFixture(p);
  // 在流中间插入一个损坏的 slice NAL（合法起始码 + 残负载）
  const size_t cut = data.size() / 2;
  std::vector<uint8_t> corrupted;
  corrupted.insert(corrupted.end(), data.begin(), data.begin() + cut);
  corrupted.push_back(0x00);
  corrupted.push_back(0x00);
  corrupted.push_back(0x01);
  corrupted.push_back(0x41);
  corrupted.push_back(0x00); // 残缺 slice
  corrupted.insert(corrupted.end(), data.begin() + cut, data.end());

  auto tolerant = analyzeBytes(corrupted, false);
  // tolerant：有 warning 但仍产出完整 GOP 结构
  CHECK(tolerant.parse_failure_count >= 1u);
  CHECK(tolerant.gops.size() >= 1u);
  CHECK(tolerant.pictures.size() >= 1u);

  auto strict = analyzeBytes(corrupted, true);
  CHECK(strict.parse_failure_count >= 1u);
}

TEST_CASE(integration_json_field_consistency) {
  auto r = analyzeBytes(generateFixture(FixtureParams{}));
  std::ostringstream out;
  ReportWriter::write(out, r, ReportFormat::JSON, false, "x.264");
  const std::string j = out.str();
  // JSON 中报告的计数必须与结构体一致
  CHECK(j.find("\"frame_count\": " + std::to_string(r.pictures.size())) !=
        std::string::npos);
  CHECK(j.find("\"gop_count\": " + std::to_string(r.gops.size())) !=
        std::string::npos);
  CHECK(j.find("\"exp_golomb_calls\": " +
                   std::to_string(r.golomb.success)) !=
        std::string::npos);
  // 每组 start_offset 均为非负整数字段
  for (const auto &g : r.gops)
    CHECK(j.find("\"start_offset\": " + std::to_string(g.start_offset)) !=
          std::string::npos);
}

TEST_CASE(integration_stream_equals_chunked_scan) {
  // 整缓冲扫描与分块流式扫描结果必须一致（内存约束的等价性）
  auto data = generateFixture(FixtureParams{});
  auto whole = NalScanner::scanAll(data.data(), data.size());
  std::istringstream in(std::string(
      reinterpret_cast<const char *>(data.data()), data.size()));
  auto chunked = NalScanner::scanStream(in, 7); // 极小分块
  CHECK_EQ(whole.size(), chunked.size());
  for (size_t i = 0; i < whole.size(); ++i) {
    CHECK_EQ(whole[i].start_offset, chunked[i].start_offset);
    CHECK_EQ(whole[i].nal_unit_type, chunked[i].nal_unit_type);
    CHECK(whole[i].ebsp == chunked[i].ebsp);
    CHECK_EQ(whole[i].truncated, chunked[i].truncated);
  }
}
