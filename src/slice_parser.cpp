#include "slice_parser.h"

#include "bit_reader.h"
#include "exp_golomb.h"

namespace h264 {

const char *SliceParser::typeName(uint8_t t) {
  switch (t) {
  case SLICE_P: return "P";
  case SLICE_B: return "B";
  case SLICE_I: return "I";
  case SLICE_SP: return "SP";
  case SLICE_SI: return "SI";
  default: return "?";
  }
}

ParseStatus SliceParser::parse(const NalUnit &nal, const SpsInfo &sps,
                               const PpsInfo &pps, SliceInfo &o) {
  if (nal.ebsp.size() < 2)
    return {false, "slice NAL too short"};

  bool had_epb = false;
  std::vector<uint8_t> rbsp =
      BitReader::removeEmulationBytes(nal.ebsp.data(), nal.ebsp.size(),
                                      had_epb);
  BitReader br(rbsp.data() + 1, rbsp.size() - 1); // 跳过 NAL 头
  ExpGolomb eg(br);
  auto fail = [&](const std::string &m) {
    return ParseStatus{false, m, eg.stats()};
  };

  if (!eg.readUE(o.first_mb_in_slice))
    return fail("slice: exhausted first_mb_in_slice");
  uint32_t raw_type = 0;
  if (!eg.readUE(raw_type))
    return fail("slice: exhausted slice_type");
  // 5..9 与 0..4 等价（表示该图像所有 slice 类型相同）
  o.slice_type = static_cast<uint8_t>(raw_type >= 5 ? raw_type - 5 : raw_type);
  o.is_i = (o.slice_type == SLICE_I || o.slice_type == SLICE_SI);
  o.is_p = (o.slice_type == SLICE_P || o.slice_type == SLICE_SP);
  o.is_b = (o.slice_type == SLICE_B);
  if (!eg.readUE(o.pps_id))
    return fail("slice: exhausted pps_id");

  const unsigned frame_num_bits = sps.log2_max_frame_num_minus4 + 4;
  uint32_t fn = 0;
  if (!br.tryReadBits(frame_num_bits, fn))
    return fail("slice: exhausted frame_num");
  o.frame_num = fn;

  if (!sps.frame_mbs_only_flag) {
    uint32_t field = 0;
    if (!br.tryReadBits(1, field))
      return fail("slice: exhausted field_pic_flag");
    o.field_pic_flag = field != 0;
    if (o.field_pic_flag) {
      uint32_t bottom = 0;
      if (!br.tryReadBits(1, bottom))
        return fail("slice: exhausted bottom_field_flag");
      o.bottom_field_flag = bottom != 0;
    }
  }

  if (nal.nal_unit_type == NAL_IDR_SLICE) {
    if (!eg.readUE(o.idr_pic_id))
      return fail("slice: exhausted idr_pic_id");
    o.idr_pic_id_present = true;
  }

  // POC（7.3.3.3）：仅消费本工具报告的字段，其余类型按规范跳过
  if (sps.pic_order_cnt_type == 0) {
    if (!br.tryReadBits(sps.log2_max_pic_order_cnt_lsb_minus4 + 4,
                        o.pic_order_cnt_lsb))
      return fail("slice: exhausted poc_lsb");
    if (pps.bottom_field_pic_order_in_frame && !o.field_pic_flag) {
      int32_t d = 0;
      if (!eg.readSE(d))
        return fail("slice: exhausted delta_poc_bottom");
      o.delta_pic_order_valid = true;
      o.delta_pic_order_cnt_bottom = d;
    }
  } else if (sps.pic_order_cnt_type == 1 && !sps.frame_mbs_only_flag) {
    uint32_t delta_pic_order_always_zero = 0;
    if (!br.tryReadBits(1, delta_pic_order_always_zero))
      return fail("slice: exhausted poc type1 flag");
    if (!delta_pic_order_always_zero) {
      int32_t d0 = 0, d1 = 0;
      if (!eg.readSE(d0))
        return fail("slice: exhausted delta_poc0");
      if (o.field_pic_flag && eg.readSE(d1))
        o.delta_pic_order_valid = true;
    }
  }
  // poc_type==2 无 POC 字段

  if (pps.redundant_pic_cnt_present) {
    if (!eg.readUE(o.redundant_pic_cnt))
      return fail("slice: exhausted redundant_pic_cnt");
    o.redundant_pic_cnt_present = true;
  }

  // 后续参考帧列表/加权预测/解码参考帧标记与结构分析无关，到此停止。
  return {true, "", eg.stats()};
}

} // namespace h264
