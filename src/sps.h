#pragma once

#include <cstdint>
#include <map>

#include "bitreader.h"

namespace h264 {

struct HrdParams {
  bool present = false;
  uint32_t cpb_cnt_minus1 = 0;
  uint32_t initial_cpb_removal_delay_length_minus1 = 23;
  uint32_t cpb_removal_delay_length_minus1 = 23;
  uint32_t dpb_output_delay_length_minus1 = 23;
  uint32_t time_offset_length = 24;
};

struct VuiParams {
  bool present = false;
  bool timing_info_present = false;
  uint32_t num_units_in_tick = 0;
  uint32_t time_scale = 0;
  bool fixed_frame_rate_flag = false;
  HrdParams nal_hrd;
  HrdParams vcl_hrd;
};

struct Sps {
  uint32_t sps_id = 0;
  uint32_t profile_idc = 0;
  uint32_t level_idc = 0;
  uint32_t chroma_format_idc = 1;
  uint32_t bit_depth_luma_minus8 = 0;
  uint32_t bit_depth_chroma_minus8 = 0;
  uint32_t log2_max_frame_num = 4;
  uint32_t pic_order_cnt_type = 0;
  uint32_t log2_max_pic_order_cnt_lsb = 4;
  bool delta_pic_order_always_zero = false;
  int32_t offset_for_non_ref_pic = 0;
  int32_t offset_for_top_to_bottom_field = 0;
  uint32_t pic_width_in_mbs = 0;         // pic_width_in_mbs_minus1 + 1
  uint32_t pic_height_in_map_units = 0;  // +1 already applied
  bool frame_mbs_only_flag = true;
  bool mb_adaptive_frame_field_flag = false;
  bool frame_cropping_flag = false;
  uint32_t crop_left = 0, crop_right = 0, crop_top = 0, crop_bottom = 0;
  VuiParams vui;

  uint32_t width() const;   // pixels, cropping applied
  uint32_t height() const;  // pixels, cropping applied
  uint32_t pic_size_in_map_units() const { return pic_width_in_mbs * pic_height_in_map_units; }
  double fps() const;  // 0 when VUI timing info is absent
};

// Parses one SPS NAL payload (EBSP, header byte excluded).
// Throws kParse on malformed bits, kUnsupported on out-of-scope features.
Sps parse_sps(const uint8_t* data, size_t size);

// Active-parameter-set registry keyed by sps_id.
class SpsStore {
public:
  void add(Sps sps) { map_[sps.sps_id] = sps; }
  const Sps* find(uint32_t id) const;
  size_t size() const { return map_.size(); }

private:
  std::map<uint32_t, Sps> map_;
};

}  // namespace h264
