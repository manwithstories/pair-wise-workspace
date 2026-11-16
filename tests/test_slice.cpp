#include "h264_test_helpers.h"
#include "slice_parser.h"
#include "sps_parser.h"
#include "test_framework.h"

#include <stdexcept>

using namespace h264;

namespace {
// 构造一个 slice header NAL（RBSP 在主字段后即收尾，结构分析到此足够）
NalUnit makeSlice(uint8_t nal_header, uint32_t first_mb, uint8_t slice_type,
                  uint32_t frame_num, const SpsInfo &sps,
                  bool field = false, bool bottom = false) {
  BitWriter w;
  w.writeUE(first_mb);
  w.writeUE(slice_type);
  w.writeUE(0); // pps id
  w.writeBits(frame_num, sps.log2_max_frame_num_minus4 + 4);
  if (!sps.frame_mbs_only_flag) {
    w.writeBit(field);
    if (field)
      w.writeBit(bottom);
  }
  if ((nal_header & 0x1Fu) == NAL_IDR_SLICE)
    w.writeUE(0); // idr_pic_id
  if (sps.pic_order_cnt_type == 0)
    w.writeBits(0, sps.log2_max_pic_order_cnt_lsb_minus4 + 4);
  w.writeRbspTrailingBits();
  return h264test::wrapNal(nal_header, w.bytes());
}

SpsInfo parseSps(bool interlaced) {
  auto nal = h264test::makeSps1080p(interlaced);
  SpsInfo s;
  auto st = SpsParser::parse(nal, s);
  if (!st.ok)
    throw std::runtime_error(st.error);
  return s;
}
} // namespace

TEST_CASE(slice_classify_i_p_b) {
  auto sps = parseSps(false);
  auto pps_nal = h264test::makePps();
  PpsInfo pps;
  CHECK(PpsParser::parse(pps_nal, pps).ok);

  auto i_nal = makeSlice(0x65, 0, SLICE_I, 0, sps); // IDR I
  auto p_nal = makeSlice(0x41, 0, SLICE_P, 1, sps);
  auto b_nal = makeSlice(0x01, 0, SLICE_B, 2, sps);

  SliceInfo si, sp, sb;
  CHECK(SliceParser::parse(i_nal, sps, pps, si).ok);
  CHECK(SliceParser::parse(p_nal, sps, pps, sp).ok);
  CHECK(SliceParser::parse(b_nal, sps, pps, sb).ok);

  CHECK(si.is_i);
  CHECK(!si.is_p);
  CHECK(sp.is_p);
  CHECK(sb.is_b);
  CHECK_EQ(si.frame_num, 0u);
  CHECK_EQ(sp.frame_num, 1u);
  CHECK_EQ(sb.frame_num, 2u);
}

TEST_CASE(slice_type_5_to_9_alias) {
  auto sps = parseSps(false);
  PpsInfo pps;
  PpsParser::parse(h264test::makePps(), pps);
  // slice_type=7 等价于 2（I）
  auto nal = makeSlice(0x65, 0, 7, 0, sps);
  SliceInfo s;
  CHECK(SliceParser::parse(nal, sps, pps, s).ok);
  CHECK(s.is_i);
  CHECK_EQ(s.slice_type, (unsigned)SLICE_I);
}

TEST_CASE(slice_multi_slice_id_and_mb) {
  auto sps = parseSps(false);
  PpsInfo pps;
  PpsParser::parse(h264test::makePps(), pps);
  // 同一图像的三个分片：first_mb 0 / 2700 / 5400
  auto s0 = makeSlice(0x65, 0, SLICE_I, 0, sps);
  auto s1 = makeSlice(0x65, 2700, SLICE_I, 0, sps);
  auto s2 = makeSlice(0x65, 5400, SLICE_I, 0, sps);
  SliceInfo i0, i1, i2;
  CHECK(SliceParser::parse(s0, sps, pps, i0).ok);
  CHECK(SliceParser::parse(s1, sps, pps, i1).ok);
  CHECK(SliceParser::parse(s2, sps, pps, i2).ok);
  CHECK_EQ(i0.first_mb_in_slice, 0u);
  CHECK_EQ(i1.first_mb_in_slice, 2700u);
  CHECK_EQ(i2.first_mb_in_slice, 5400u);
  CHECK_EQ(i0.frame_num, i1.frame_num);
  CHECK_EQ(i1.frame_num, i2.frame_num);
}

TEST_CASE(slice_interlaced_field_flags) {
  auto sps = parseSps(true); // mbaff 隔行
  PpsInfo pps;
  PpsParser::parse(h264test::makePps(), pps);

  auto top = makeSlice(0x65, 0, SLICE_I, 0, sps, /*field=*/true,
                       /*bottom=*/false);
  auto bot = makeSlice(0x65, 0, SLICE_I, 0, sps, /*field=*/true,
                       /*bottom=*/true);
  SliceInfo st, sb;
  CHECK(SliceParser::parse(top, sps, pps, st).ok);
  CHECK(SliceParser::parse(bot, sps, pps, sb).ok);
  CHECK(st.field_pic_flag);
  CHECK(!st.bottom_field_flag);
  CHECK(sb.field_pic_flag);
  CHECK(sb.bottom_field_flag);
}

TEST_CASE(slice_truncated_returns_error) {
  auto sps = parseSps(false);
  PpsInfo pps;
  PpsParser::parse(h264test::makePps(), pps);
  NalUnit bad;
  bad.ebsp = {0x41, 0x00, 0x00}; // 头后内容不足以读 frame_num
  SliceInfo s;
  auto st = SliceParser::parse(bad, sps, pps, s);
  CHECK(!st.ok);
  CHECK(!st.error.empty());
}
