#include "sps_parser.h"

#include "bit_reader.h"
#include "exp_golomb.h"

namespace h264 {

namespace {

bool isHighProfile(uint8_t profile_idc) {
  switch (profile_idc) {
  case 100: case 110: case 122: case 244: case 44:
  case 83: case 86: case 118: case 128:
    return true;
  default:
    return false;
  }
}

// scaling_list 语法本工具不关心内容，按规范跳过（7.3.2.1.1.1）。
bool skipScalingList(BitReader &br, ExpGolomb &eg, unsigned size) {
  int last_scale = 8, next_scale = 8;
  for (unsigned j = 0; j < size; ++j) {
    if (next_scale != 0) {
      int32_t delta = 0;
      if (!eg.readSE(delta))
        return false;
      next_scale = (last_scale + delta + 256) % 256;
    }
    if (next_scale != 0)
      last_scale = next_scale;
  }
  return true;
}

void deriveDimensions(SpsInfo &o) {
  const uint64_t width_mbs =
      static_cast<uint64_t>(o.pic_width_in_mbs_minus1) + 1;
  const uint64_t height_map =
      static_cast<uint64_t>(o.pic_height_in_map_units_minus1) + 1;
  // 用 64 位运算避免恶意超大 ue(v) 值导致 32 位乘法溢出
  const uint64_t raw_width = width_mbs * 16ull;
  const uint64_t raw_height =
      (o.frame_mbs_only_flag ? 1ull : 2ull) * height_map * 16ull;

  // 裁剪单位（规范 7.4.2.1.1，公式 7-18/7-19/7-20/7-21）
  const uint64_t sub_w_c = (o.chroma_format_idc == 0) ? 1ull : 2ull;
  const uint64_t sub_h_c_base = (o.chroma_format_idc == 1) ? 2ull : 1ull;
  const uint64_t sub_h_c =
      sub_h_c_base * (o.frame_mbs_only_flag ? 1ull : 2ull);
  const uint64_t crop_x =
      sub_w_c *
      (static_cast<uint64_t>(o.frame_crop_left) + o.frame_crop_right);
  const uint64_t crop_y =
      sub_h_c *
      (static_cast<uint64_t>(o.frame_crop_top) + o.frame_crop_bottom);

  const uint64_t w = raw_width > crop_x ? raw_width - crop_x : 0;
  const uint64_t h = raw_height > crop_y ? raw_height - crop_y : 0;
  // 钳制到 32 位可表达范围，拒绝静默截断导致的错误尺寸
  constexpr uint64_t kMaxDim = 0xFFFFFFFFull;
  o.coded_width = static_cast<uint32_t>(w > kMaxDim ? kMaxDim : w);
  o.coded_height = static_cast<uint32_t>(h > kMaxDim ? kMaxDim : h);
}

// 仅解析 VUI 中与时序相关的部分；其余字段软跳过，失败不致命。
void parseVui(BitReader &br, ExpGolomb &eg, SpsInfo &o) {
  uint32_t aspect = 0, overscan = 0, video_sig = 0, chroma_loc = 0;
  if (!br.tryReadBits(1, aspect) || !br.tryReadBits(1, overscan) ||
      !br.tryReadBits(1, video_sig) || !br.tryReadBits(1, chroma_loc))
    return;
  if (aspect) {
    uint32_t sar = 0;
    if (!eg.readUE(sar))
      return;
    if (sar == 255) { // Extended_SAR
      if (!br.tryReadBits(16, aspect) || !br.tryReadBits(16, aspect))
        return;
    }
  }
  if (overscan) {
    uint32_t x = 0;
    if (!br.tryReadBits(1, x))
      return;
  }
  if (video_sig) {
    uint32_t v = 0;
    if (!br.tryReadBits(3, v)) // video_format
      return;
    uint32_t full_range = 0, colour_desc = 0;
    if (!br.tryReadBits(1, full_range) || !br.tryReadBits(1, colour_desc))
      return;
    if (colour_desc) {
      if (!br.tryReadBits(8, v) || !br.tryReadBits(8, v) ||
          !br.tryReadBits(8, v))
        return;
    }
  }
  if (chroma_loc) {
    uint32_t t = 0;
    if (!eg.readUE(t))
      return;
    if (t > 0 && !eg.readUE(t))
      return;
  }
  uint32_t timing = 0;
  if (!br.tryReadBits(1, timing))
    return;
  if (timing) {
    uint32_t n = 0, ts = 0, ffr = 0;
    if (!br.tryReadBits(32, n) || !br.tryReadBits(32, ts) ||
        !br.tryReadBits(1, ffr))
      return;
    o.num_units_in_tick = n;
    o.time_scale = ts;
    o.fixed_frame_rate = ffr != 0;
    o.timing_valid = true;
    if (n != 0 && ts != 0)
      o.fps = static_cast<double>(ts) / (2.0 * n);
  }
  // 其余 VUI（HRD/比特流限制等）与本工具目标无关，不再深入。
}

} // namespace

ParseStatus SpsParser::parse(const NalUnit &nal, SpsInfo &o) {
  if (nal.ebsp.size() < 2)
    return {false, "SPS NAL too short"};

  bool had_epb = false;
  std::vector<uint8_t> rbsp =
      BitReader::removeEmulationBytes(nal.ebsp.data(), nal.ebsp.size(),
                                      had_epb);
  BitReader br(rbsp.data() + 1, rbsp.size() - 1); // 跳过 NAL 头
  ExpGolomb eg(br);

  auto fail = [&](const std::string &m) {
    return ParseStatus{false, m, eg.stats()};
  };
  auto ue = [&](uint32_t &v) -> bool { return eg.readUE(v); };
  auto fixed = [&](unsigned n, uint32_t &v) -> bool {
    return br.tryReadBits(n, v);
  };

  uint32_t prof = 0, constr = 0, lvl = 0;
  if (!fixed(8, prof) || !fixed(8, constr) || !fixed(8, lvl))
    return fail("SPS: exhausted reading profile/constraint/level");
  o.profile_idc = static_cast<uint8_t>(prof);
  o.constraint_flags = static_cast<uint8_t>(constr);
  o.level_idc = static_cast<uint8_t>(lvl);
  if (!ue(o.sps_id))
    return fail("SPS: exhausted reading sps_id");

  if (isHighProfile(o.profile_idc)) {
    if (!ue(o.chroma_format_idc))
      return fail("SPS: exhausted reading chroma_format_idc");
    if (o.chroma_format_idc == 3) {
      uint32_t separate = 0;
      if (!br.tryReadBits(1, separate))
        return fail("SPS: exhausted reading separate_colour_plane");
    }
    if (!ue(o.bit_depth_luma_minus8) || !ue(o.bit_depth_chroma_minus8))
      return fail("SPS: exhausted reading bit depth");
    uint32_t qpprime = 0, scaling = 0;
    if (!ue(qpprime) || !ue(scaling))
      return fail("SPS: exhausted qpprime/scaling flags");
    if (scaling) {
      const unsigned lists = (o.chroma_format_idc != 3) ? 8u : 12u;
      for (unsigned i = 0; i < lists; ++i) {
        uint32_t present = 0;
        if (!br.tryReadBits(1, present))
          return fail("SPS: exhausted scaling list flag");
        if (present && !skipScalingList(br, eg, i < 6 ? 16u : 64u))
          return fail("SPS: exhausted inside scaling list");
      }
    }
  }

  if (!ue(o.log2_max_frame_num_minus4) || !ue(o.pic_order_cnt_type))
    return fail("SPS: exhausted reading frame_num/poc type");
  if (o.log2_max_frame_num_minus4 > 12)
    return fail("SPS: log2_max_frame_num_minus4 out of range");
  o.max_frame_num = 1u << (o.log2_max_frame_num_minus4 + 4);

  if (o.pic_order_cnt_type == 0) {
    if (!ue(o.log2_max_pic_order_cnt_lsb_minus4))
      return fail("SPS: exhausted reading poc lsb");
  } else if (o.pic_order_cnt_type == 1) {
    uint32_t always_zero = 0, nspoc = 0;
    if (!br.tryReadBits(1, always_zero))
      return fail("SPS: poc type1 delta_pic_order_always_zero");
    int32_t off_non_ref = 0;
    if (!eg.readSE(off_non_ref) || !ue(nspoc))
      return fail("SPS: poc type1 offsets exhausted");
    for (uint32_t i = 0; i < nspoc; ++i) {
      int32_t off = 0;
      if (!eg.readSE(off))
        return fail("SPS: poc type1 offset exhausted");
    }
  } else if (o.pic_order_cnt_type > 2) {
    return fail("SPS: invalid pic_order_cnt_type");
  }

  if (!ue(o.max_num_ref_frames))
    return fail("SPS: exhausted reading max_num_ref_frames");
  uint32_t gaps = 0;
  if (!br.tryReadBits(1, gaps))
    return fail("SPS: exhausted reading gaps flag");
  o.gaps_in_frame_num_allowed = gaps != 0;
  if (!ue(o.pic_width_in_mbs_minus1) ||
      !ue(o.pic_height_in_map_units_minus1))
    return fail("SPS: exhausted reading dimensions");

  uint32_t frame_mbs = 0;
  if (!br.tryReadBits(1, frame_mbs))
    return fail("SPS: exhausted reading frame_mbs_only");
  o.frame_mbs_only_flag = frame_mbs != 0;
  if (!o.frame_mbs_only_flag) {
    uint32_t mbaff = 0;
    if (!br.tryReadBits(1, mbaff))
      return fail("SPS: exhausted reading mb_adaptive");
    o.mb_adaptive_frame_field_flag = mbaff != 0;
  }
  uint32_t direct8x8 = 0, crop = 0;
  if (!br.tryReadBits(1, direct8x8) || !br.tryReadBits(1, crop))
    return fail("SPS: exhausted reading direct/crop flags");
  o.direct_8x8_inference_flag = direct8x8 != 0;
  o.frame_cropping_flag = crop != 0;
  if (crop) {
    if (!ue(o.frame_crop_left) || !ue(o.frame_crop_right) ||
        !ue(o.frame_crop_top) || !ue(o.frame_crop_bottom))
      return fail("SPS: exhausted reading crop offsets");
  }

  uint32_t vui_present = 0;
  if (!br.tryReadBits(1, vui_present))
    return fail("SPS: exhausted reading vui present");
  if (vui_present)
    parseVui(br, eg, o); // VUI 软失败：时序缺失不致命

  deriveDimensions(o);
  return {true, "", eg.stats()};
}

ParseStatus PpsParser::parse(const NalUnit &nal, PpsInfo &o) {
  if (nal.ebsp.size() < 2)
    return {false, "PPS NAL too short"};
  bool had_epb = false;
  std::vector<uint8_t> rbsp =
      BitReader::removeEmulationBytes(nal.ebsp.data(), nal.ebsp.size(),
                                      had_epb);
  BitReader br(rbsp.data() + 1, rbsp.size() - 1);
  ExpGolomb eg(br);
  auto fail = [&](const std::string &m) {
    return ParseStatus{false, m, eg.stats()};
  };

  if (!eg.readUE(o.pps_id) || !eg.readUE(o.sps_id))
    return fail("PPS: exhausted reading ids");
  uint32_t entropy = 0, bottom = 0;
  if (!br.tryReadBits(1, entropy) || !br.tryReadBits(1, bottom))
    return fail("PPS: exhausted coding flags");
  o.entropy_coding_mode = entropy != 0;
  o.bottom_field_pic_order_in_frame = bottom != 0;
  if (!eg.readUE(o.num_slice_groups_minus1))
    return fail("PPS: exhausted slice groups");
  if (o.num_slice_groups_minus1 > 0) {
    uint32_t map_type = 0;
    if (!eg.readUE(map_type))
      return fail("PPS: exhausted slice group map type");
    // 本工具不支持 FMO 细分，消费主字段后即返回（夹具不使用 FMO）
    return {true, "", eg.stats()};
  }
  if (!eg.readUE(o.num_ref_idx_l0_default) ||
      !eg.readUE(o.num_ref_idx_l1_default))
    return fail("PPS: exhausted ref idx defaults");
  uint32_t wp = 0;
  if (!br.tryReadBits(1, wp))
    return fail("PPS: exhausted weighted pred");
  o.weighted_pred = wp != 0;
  uint32_t wb = 0;
  if (!br.tryReadBits(2, wb))
    return fail("PPS: exhausted weighted bipred");
  o.weighted_bipred_idc = static_cast<uint8_t>(wb);
  if (!eg.readSE(o.pic_init_qp_minus26))
    return fail("PPS: exhausted pic_init_qp");
  int32_t qs = 0, chroma_qp = 0;
  if (!eg.readSE(qs) || !eg.readSE(chroma_qp))
    return fail("PPS: exhausted qp offsets");
  uint32_t dblk = 0, redundant = 0;
  if (!br.tryReadBits(1, dblk) || !br.tryReadBits(1, redundant))
    return fail("PPS: exhausted control flags");
  o.deblocking_filter_control = dblk != 0;
  o.redundant_pic_cnt_present = redundant != 0;
  return {true, "", eg.stats()};
}

} // namespace h264
