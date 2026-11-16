#include "gop_analyzer.h"
#include "h264_test_helpers.h"
#include "slice_parser.h"
#include "sps_parser.h"
#include "test_framework.h"

#include <stdexcept>

using namespace h264;

namespace {
SpsInfo mustSps(bool interlaced) {
  SpsInfo s;
  auto st = SpsParser::parse(h264test::makeSps1080p(interlaced), s);
  if (!st.ok)
    throw std::runtime_error(st.error);
  return s;
}
PpsInfo mustPps() {
  PpsInfo p;
  auto st = PpsParser::parse(h264test::makePps(), p);
  if (!st.ok)
    throw std::runtime_error(st.error);
  return p;
}

// 构造一个分片 NAL，并赋予指定起始偏移（结构分析只依赖偏移字段）
NalUnit sliceNal(uint8_t header, uint32_t first_mb, uint8_t type,
                 uint32_t frame_num, const SpsInfo &sps, uint64_t offset,
                 bool field = false, bool bottom = false) {
  BitWriter w;
  w.writeUE(first_mb);
  w.writeUE(type);
  w.writeUE(0);
  w.writeBits(frame_num, sps.log2_max_frame_num_minus4 + 4);
  if (!sps.frame_mbs_only_flag) {
    w.writeBit(field);
    if (field)
      w.writeBit(bottom);
  }
  if ((header & 0x1Fu) == NAL_IDR_SLICE)
    w.writeUE(0);
  if (sps.pic_order_cnt_type == 0)
    w.writeBits(0, sps.log2_max_pic_order_cnt_lsb_minus4 + 4);
  w.writeRbspTrailingBits();
  auto nal = h264test::wrapNal(header, w.bytes());
  nal.start_offset = offset;
  return nal;
}
} // namespace

TEST_CASE(gop_three_groups_with_stable_offsets) {
  auto sps = mustSps(false);
  GopAnalyzer a;
  a.processNal(h264test::makeSps1080p(false));
  a.processNal(h264test::makePps());

  // GOP0: IDR-I(3 slices) + P；GOP1: I + B；GOP2: IDR-I
  uint64_t off = 100;
  a.processNal(sliceNal(0x65, 0, SLICE_I, 0, sps, off));        // I 分片0
  a.processNal(sliceNal(0x65, 2700, SLICE_I, 0, sps, off + 10));
  a.processNal(sliceNal(0x65, 5400, SLICE_I, 0, sps, off + 20));
  a.processNal(sliceNal(0x41, 0, SLICE_P, 1, sps, off + 30));
  a.processNal(sliceNal(0x21, 0, SLICE_I, 2, sps, off + 40));   // 非 IDR I（NAL type 1）
  a.processNal(sliceNal(0x01, 0, SLICE_B, 3, sps, off + 50));
  a.processNal(sliceNal(0x65, 0, SLICE_I, 4, sps, off + 60));   // IDR
  auto r = a.finish();

  CHECK_EQ(r.gops.size(), 3u);
  CHECK_EQ(r.pictures.size(), 5u);
  CHECK_EQ(r.gops[0].start_offset, 100u);
  CHECK(r.gops[0].starts_with_idr);
  CHECK_EQ(r.gops[0].picture_count, 2u);
  CHECK_EQ(r.gops[0].i_count, 1u);
  CHECK_EQ(r.gops[0].p_count, 1u);
  CHECK_EQ(r.gops[1].start_offset, 140u);
  CHECK(!r.gops[1].starts_with_idr);
  CHECK_EQ(r.gops[1].b_count, 1u);
  CHECK_EQ(r.gops[2].start_offset, 160u);

  // 多分片图像：I 帧 3 片，其余 1 片
  CHECK_EQ(r.min_slices_per_pic, 1u);
  CHECK_EQ(r.max_slices_per_pic, 3u);
  CHECK_EQ(r.slice_count, 7u);
  for (const auto &g : r.gops)
    CHECK(g.start_offset < 1000000u); // 非负整数稳定性
}

TEST_CASE(gop_multi_slice_merges_one_picture) {
  auto sps = mustSps(false);
  GopAnalyzer a;
  a.processNal(h264test::makeSps1080p(false));
  a.processNal(h264test::makePps());
  // 同一 frame_num=0 的 3 个分片必须合并为 1 个图像
  a.processNal(sliceNal(0x65, 0, SLICE_I, 0, sps, 0));
  a.processNal(sliceNal(0x65, 100, SLICE_I, 0, sps, 5));
  a.processNal(sliceNal(0x65, 200, SLICE_I, 0, sps, 9));
  auto r = a.finish();
  CHECK_EQ(r.pictures.size(), 1u);
  CHECK_EQ(r.pictures[0].slice_count, 3u);
  CHECK_EQ(r.pictures[0].first_mb_min, 0u);
  CHECK_EQ(r.pictures[0].first_mb_max, 200u);
  CHECK_EQ(r.gops.size(), 1u);
}

TEST_CASE(gop_interlaced_field_pairs) {
  auto sps = mustSps(true);
  GopAnalyzer a;
  a.processNal(h264test::makeSps1080p(true));
  a.processNal(h264test::makePps());
  // frame 0：顶场+底场（完整对）；frame 1：仅顶场（缺失配对）
  a.processNal(sliceNal(0x65, 0, SLICE_I, 0, sps, 0, true, false));
  a.processNal(sliceNal(0x65, 0, SLICE_I, 0, sps, 8, true, true));
  a.processNal(sliceNal(0x41, 0, SLICE_P, 1, sps, 16, true, false));
  auto r = a.finish();
  CHECK_EQ(r.field_pairs.size(), 2u);
  CHECK_EQ(r.field_pair_complete, 1u);
  CHECK_EQ(r.field_pair_incomplete, 1u);
  CHECK(r.field_pairs[0].top_present && r.field_pairs[0].bottom_present);
  CHECK(r.field_pairs[1].top_present && !r.field_pairs[1].bottom_present);
}

TEST_CASE(gop_broken_slice_does_not_abort_stream) {
  auto sps = mustSps(false);
  GopAnalyzer tolerant(false);
  tolerant.processNal(h264test::makeSps1080p(false));
  tolerant.processNal(h264test::makePps());
  tolerant.processNal(sliceNal(0x65, 0, SLICE_I, 0, sps, 0));
  NalUnit broken;
  broken.ebsp = {0x41, 0x00}; // 截断的 P slice
  broken.nal_unit_type = NAL_SLICE;
  broken.start_offset = 50;
  tolerant.processNal(broken); // 计入异常但不中止
  tolerant.processNal(sliceNal(0x41, 0, SLICE_P, 1, sps, 60)); // 后续合法
  auto r = tolerant.finish();
  CHECK_EQ(r.parse_failure_count, 1u);
  CHECK_EQ(r.warnings.size(), 1u);
  CHECK_EQ(r.pictures.size(), 2u); // I + 后续 P 仍被聚合
  CHECK_EQ(r.gops.size(), 1u);
}

TEST_CASE(gop_strict_mode_reports_failure) {
  GopAnalyzer strict(/*strict=*/true);
  strict.processNal(h264test::makeSps1080p(false));
  // 无 PPS 直接喂 slice：strict 返回 false
  auto sps = mustSps(false);
  const bool ok = strict.processNal(sliceNal(0x65, 0, SLICE_I, 0, sps, 0));
  CHECK(!ok);
  auto r = strict.finish();
  CHECK_EQ(r.parse_failure_count, 1u);
}
