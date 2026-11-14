#include "slice.h"

namespace h264 {

FrameType normalize_slice_type(uint32_t st) {
  if (st > 9) {
    fail_parse("slice_type out of range: " + std::to_string(st));
  }
  switch (st) {
    case 2: case 5: case 7: return FrameType::kI;
    case 0: return FrameType::kP;
    case 1: case 6: return FrameType::kB;
    default: return FrameType::kOther;  // 3, 4, 8, 9: SP/SI
  }
}

const char* frame_type_name(FrameType t) {
  switch (t) {
    case FrameType::kI: return "I";
    case FrameType::kP: return "P";
    case FrameType::kB: return "B";
    default: return "other";
  }
}

namespace {

class UeTracker {
public:
  explicit UeTracker(BitReader& br) : br_(br) {}
  uint32_t ue() { return track(br_.read_ue()); }
  int32_t se() { return track(br_.read_se()); }

  uint64_t count = 0;
  uint64_t bits = 0;

private:
  template <typename T>
  T track(T v) {
    ++count;
    bits += static_cast<uint64_t>(br_.last_ue_bits());
    return v;
  }
  BitReader& br_;
};

}  // namespace

SliceHeader parse_slice_header(const uint8_t* data, size_t size, bool idr,
                               const PpsStore& pps_store, const SpsStore& sps_store) {
  BitReader br(data, size);
  UeTracker ue(br);
  SliceHeader h;
  h.idr = idr;
  h.first_mb_in_slice = ue.ue();
  h.slice_type = ue.ue();
  h.frame_type = normalize_slice_type(h.slice_type);
  h.pps_id = ue.ue();
  h.pps = pps_store.find(h.pps_id);
  if (!h.pps) {
    fail_parse("slice references missing PPS " + std::to_string(h.pps_id));
  }
  h.sps = sps_store.find(h.pps->sps_id);
  if (!h.sps) {
    fail_parse("PPS " + std::to_string(h.pps_id) + " references missing SPS " +
               std::to_string(h.pps->sps_id));
  }
  const Sps& sps = *h.sps;
  const Pps& pps = *h.pps;
  h.frame_num = br.read_bits(static_cast<int>(sps.log2_max_frame_num));
  if (!sps.frame_mbs_only_flag) {
    h.field_pic_flag = br.read_bit() != 0;
    if (h.field_pic_flag) {
      h.bottom_field_flag = br.read_bit() != 0;
    }
  }
  if (idr) {
    h.idr_pic_id = ue.ue();
  }
  if (sps.pic_order_cnt_type == 0) {
    h.pic_order_cnt_lsb = br.read_bits(static_cast<int>(sps.log2_max_pic_order_cnt_lsb));
    if (pps.bottom_field_pic_order_in_frame_present_flag && !h.field_pic_flag) {
      ue.se();  // delta_pic_order_cnt_bottom
    }
  } else if (sps.pic_order_cnt_type == 1 && !sps.delta_pic_order_always_zero) {
    ue.se();  // delta_pic_order_cnt[0]
    if (pps.bottom_field_pic_order_in_frame_present_flag && !h.field_pic_flag) {
      ue.se();  // delta_pic_order_cnt[1]
    }
  }
  if (pps.redundant_pic_cnt_present_flag) {
    ue.ue();  // redundant_pic_cnt
  }
  h.header_bits = br.bit_position();
  h.ue_count = ue.count;
  h.ue_bits = ue.bits;
  return h;
}

}  // namespace h264
