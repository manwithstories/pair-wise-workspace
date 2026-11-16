#include "bit_writer.h"
#include "nal_scanner.h"
#include "sps_parser.h"
#include "test_framework.h"

#include <vector>

using namespace h264;

namespace {
// 把 RBSP 包成一个 NalUnit（含 NAL 头 + 防竞争字节），便于直接喂解析器
NalUnit wrapNal(uint8_t header, const std::vector<uint8_t> &rbsp) {
  NalUnit nal;
  nal.ebsp.push_back(header);
  auto body = BitWriter::addEmulationBytes(rbsp);
  nal.ebsp.insert(nal.ebsp.end(), body.begin(), body.end());
  nal.nal_unit_type = header & 0x1Fu;
  nal.nal_ref_idc = (header >> 5) & 3u;
  nal.start_offset = 0;
  nal.header_offset = 0;
  return nal;
}

// 构造一个 High profile SPS：1920x1080、frame_mbs_only、VUI 30fps。
// 1080 非 16 倍数：编码为 1088 宏块高，再用 crop 裁掉底部 8 行。
NalUnit makeHighSps1080p(bool interlaced = false, bool extra_crop = false) {
  BitWriter w;
  w.writeBits(100, 8); // profile_idc = High
  w.writeBits(0, 8);   // constraint flags
  w.writeBits(40, 8);  // level_idc = 4.0
  w.writeUE(0);        // sps_id
  // high profile 扩展
  w.writeUE(1); // chroma_format_idc = 4:2:0
  w.writeUE(0); // bit_depth_luma_minus8
  w.writeUE(0); // bit_depth_chroma_minus8
  w.writeUE(0); // qpprime_y_zero_transform_bypass
  w.writeUE(0); // seq_scaling_matrix_present
  w.writeUE(0); // log2_max_frame_num_minus4 -> max_frame_num=16
  w.writeUE(0); // pic_order_cnt_type = 0
  w.writeUE(4); // log2_max_pic_order_cnt_lsb_minus4
  w.writeUE(2); // max_num_ref_frames
  w.writeBit(false); // gaps_in_frame_num
  // 1920/16 - 1 = 119；1088/16 - 1 = 67
  w.writeUE(119);
  w.writeUE(67);
  w.writeBit(!interlaced); // frame_mbs_only_flag
  if (interlaced)
    w.writeBit(true); // mb_adaptive_frame_field_flag
  w.writeBit(true);  // direct_8x8_inference
  w.writeBit(true);  // frame_cropping_flag（1088 -> 1080）
  w.writeUE(0);      // crop left
  w.writeUE(0);      // crop right
  w.writeUE(0);      // crop top
  // 4:2:0 逐行 sub_h_c=2：bottom=4 减 8 行得 1080；extra_crop 再减 8 行
  w.writeUE(extra_crop ? 8u : 4u);
  // VUI
  w.writeBit(true); // vui_parameters_present
  w.writeBit(false); // aspect_ratio_info_present
  w.writeBit(false); // overscan
  w.writeBit(false); // video_signal_type
  w.writeBit(false); // chroma_loc
  w.writeBit(true);  // timing_info_present
  w.writeBits(1001, 32);  // num_units_in_tick
  w.writeBits(60060, 32); // time_scale -> 30.0 fps
  w.writeBit(true);       // fixed_frame_rate
  w.writeRbspTrailingBits();
  return wrapNal(0x67, w.bytes());
}
} // namespace

TEST_CASE(sps_parse_high_profile_1080p) {
  auto nal = makeHighSps1080p();
  SpsInfo s;
  auto st = SpsParser::parse(nal, s);
  CHECK(st.ok);
  CHECK_EQ(s.profile_idc, 100);
  CHECK_EQ(s.level_idc, 40);
  CHECK_EQ(s.coded_width, 1920u);
  CHECK_EQ(s.coded_height, 1080u);
  CHECK(s.frame_mbs_only_flag);
  CHECK(!s.mb_adaptive_frame_field_flag);
  CHECK_EQ(s.max_num_ref_frames, 2u);
  CHECK_EQ(s.max_frame_num, 16u);
  CHECK(s.timing_valid);
  CHECK(s.fixed_frame_rate);
  CHECK(s.fps > 29.9 && s.fps < 30.1);
}

TEST_CASE(sps_parse_interlaced_flags) {
  auto nal = makeHighSps1080p(/*interlaced=*/true);
  SpsInfo s;
  CHECK(SpsParser::parse(nal, s).ok);
  CHECK(!s.frame_mbs_only_flag);
  CHECK(s.mb_adaptive_frame_field_flag);
  // 场编码：raw = 2*68*16 = 2176，sub_h_c=4、bottom=4 裁剪 16 行
  CHECK_EQ(s.coded_height, 2160u);
}

TEST_CASE(sps_parse_frame_cropping) {
  auto nal = makeHighSps1080p(false, /*extra_crop=*/true);
  SpsInfo s;
  CHECK(SpsParser::parse(nal, s).ok);
  CHECK(s.frame_cropping_flag);
  // raw 1088；bottom=8、sub_h_c=2 -> 减 16 行
  CHECK_EQ(s.coded_width, 1920u);
  CHECK_EQ(s.coded_height, 1072u);
}

TEST_CASE(sps_baseline_profile_no_extension) {
  BitWriter w;
  w.writeBits(66, 8);  // Baseline
  w.writeBits(0, 8);
  w.writeBits(30, 8);  // level 3.0
  w.writeUE(0);        // sps_id
  w.writeUE(0);        // log2_max_frame_num
  w.writeUE(2);        // poc type 2（无额外字段）
  w.writeUE(1);        // max ref frames
  w.writeBit(false);
  w.writeUE(79);       // 1280 宽
  w.writeUE(44);       // 720 高
  w.writeBit(true);    // frame_mbs_only
  w.writeBit(true);    // direct 8x8
  w.writeBit(false);   // crop
  w.writeBit(false);   // vui
  w.writeRbspTrailingBits();
  auto nal = wrapNal(0x67, w.bytes());
  SpsInfo s;
  CHECK(SpsParser::parse(nal, s).ok);
  CHECK_EQ(s.profile_idc, 66);
  CHECK_EQ(s.level_idc, 30);
  CHECK_EQ(s.pic_order_cnt_type, 2u);
  CHECK_EQ(s.coded_width, 1280u);
  CHECK_EQ(s.coded_height, 720u);
}

TEST_CASE(sps_large_dimensions_use_wide_arithmetic) {
  // 32 位码长约束下最大可解码 codeNum=65534（31 位码字）；验证大尺寸走
  // 64 位运算且结果正确（65535 个宏块 *16 = 1048560 像素），不回绕。
  const uint32_t big_mbs = 65534;
  BitWriter w;
  w.writeBits(100, 8);
  w.writeBits(0, 8);
  w.writeBits(40, 8);
  w.writeUE(0);
  w.writeUE(1);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(0);
  w.writeUE(4);
  w.writeUE(1);
  w.writeBit(false);
  w.writeUE(big_mbs);
  w.writeUE(big_mbs);
  w.writeBit(true);
  w.writeBit(true);
  w.writeBit(false);
  w.writeBit(false);
  w.writeRbspTrailingBits();
  auto nal = wrapNal(0x67, w.bytes());
  SpsInfo s;
  CHECK(SpsParser::parse(nal, s).ok);
  CHECK_EQ(s.pic_width_in_mbs_minus1, big_mbs);
  CHECK_EQ(s.coded_width, (big_mbs + 1u) * 16u);
  CHECK_EQ(s.coded_height, (big_mbs + 1u) * 16u);
}

TEST_CASE(sps_truncated_returns_error_not_throw) {
  NalUnit nal;
  nal.ebsp = {0x67, 100, 0}; // 头后几乎无内容
  SpsInfo s;
  auto st = SpsParser::parse(nal, s);
  CHECK(!st.ok);
  CHECK(!st.error.empty());
}

TEST_CASE(pps_parse_basic) {
  BitWriter w;
  w.writeUE(0);  // pps_id
  w.writeUE(0);  // sps_id
  w.writeBit(false); // entropy (CAVLC)
  w.writeBit(false); // bottom field poc
  w.writeUE(0);  // num slice groups
  w.writeUE(0);  // l0 default
  w.writeUE(0);  // l1 default
  w.writeBit(false); // weighted pred
  w.writeBits(0, 2); // weighted bipred idc
  w.writeSE(-26 + 23); // pic_init_qp_minus26 = -3 -> qp 23
  w.writeSE(0);  // pic_init_qs
  w.writeSE(0);  // chroma qp offset
  w.writeBit(true);  // deblocking control
  w.writeBit(false); // redundant pic cnt
  w.writeRbspTrailingBits();
  auto nal = wrapNal(0x68, w.bytes());
  PpsInfo p;
  auto st = PpsParser::parse(nal, p);
  CHECK(st.ok);
  CHECK_EQ(p.pps_id, 0u);
  CHECK_EQ(p.sps_id, 0u);
  CHECK(!p.entropy_coding_mode);
  CHECK_EQ(p.pic_init_qp_minus26, -3);
  CHECK(p.deblocking_filter_control);
}
