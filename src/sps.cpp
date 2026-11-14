#include "sps.h"

namespace h264 {

namespace {

bool profile_has_extended_format(uint32_t p) {
  switch (p) {
    case 100: case 110: case 122: case 244: case 44: case 83:
    case 86: case 118: case 128: case 138: case 139: case 134: case 135:
      return true;
    default:
      return false;
  }
}

bool profile_supported(uint32_t p) {
  switch (p) {
    case 66: case 77: case 88: case 100: case 110: case 122: case 244:
      return true;
    default:
      return false;
  }
}

void skip_scaling_list(BitReader& br, int size) {
  int32_t last_scale = 8, next_scale = 8;
  for (int j = 0; j < size; ++j) {
    if (next_scale != 0) {
      next_scale = (last_scale + br.read_se() + 256) % 256;
    }
    last_scale = (next_scale == 0) ? last_scale : next_scale;
  }
}

void parse_hrd(BitReader& br, HrdParams& hrd) {
  hrd.present = true;
  hrd.cpb_cnt_minus1 = br.read_ue();
  br.read_bits(4);  // bit_rate_scale
  br.read_bits(4);  // cpb_size_scale
  for (uint32_t i = 0; i <= hrd.cpb_cnt_minus1; ++i) {
    br.read_ue();  // bit_rate_value_minus1
    br.read_ue();  // cpb_size_value_minus1
    br.read_bit(); // cbr_flag
  }
  hrd.initial_cpb_removal_delay_length_minus1 = br.read_bits(5);
  hrd.cpb_removal_delay_length_minus1 = br.read_bits(5);
  hrd.dpb_output_delay_length_minus1 = br.read_bits(5);
  hrd.time_offset_length = br.read_bits(5);
}

void parse_vui(BitReader& br, VuiParams& vui) {
  vui.present = true;
  if (br.read_bit()) {  // aspect_ratio_info_present_flag
    uint32_t idc = br.read_bits(8);
    if (idc == 255) {
      br.read_bits(16);
      br.read_bits(16);
    }
  }
  if (br.read_bit()) {  // overscan_info_present_flag
    br.read_bit();
  }
  if (br.read_bit()) {  // video_signal_type_present_flag
    br.read_bits(3);
    br.read_bit();
    if (br.read_bit()) {  // colour_description_present_flag
      br.read_bits(24);
    }
  }
  if (br.read_bit()) {  // chroma_loc_info_present_flag
    br.read_ue();
    br.read_ue();
  }
  vui.timing_info_present = br.read_bit() != 0;
  if (vui.timing_info_present) {
    vui.num_units_in_tick = br.read_bits(32);
    vui.time_scale = br.read_bits(32);
    vui.fixed_frame_rate_flag = br.read_bit() != 0;
  }
  if (br.read_bit()) {
    parse_hrd(br, vui.nal_hrd);
  }
  if (br.read_bit()) {
    parse_hrd(br, vui.vcl_hrd);
  }
  if (vui.nal_hrd.present || vui.vcl_hrd.present) {
    br.read_bit();  // low_delay_hrd_flag
  }
  br.read_bit();  // pic_struct_present_flag
  if (br.read_bit()) {  // bitstream_restriction_flag
    br.read_bit();
    br.read_ue();
    br.read_ue();
    br.read_ue();
    br.read_ue();
    br.read_ue();
    br.read_ue();
  }
}

}  // namespace

uint32_t Sps::width() const {
  uint32_t sub_width = (chroma_format_idc == 1 || chroma_format_idc == 2) ? 2 : 1;
  uint32_t crop_x = (chroma_format_idc == 0) ? 1 : sub_width;
  return pic_width_in_mbs * 16 - (crop_left + crop_right) * crop_x;
}

uint32_t Sps::height() const {
  uint32_t sub_height = (chroma_format_idc == 1) ? 2 : 1;
  uint32_t crop_y = (chroma_format_idc == 0) ? 1 : sub_height;
  crop_y *= frame_mbs_only_flag ? 1 : 2;
  return pic_height_in_map_units * 16 * (frame_mbs_only_flag ? 1 : 2) -
         (crop_top + crop_bottom) * crop_y;
}

double Sps::fps() const {
  if (!vui.timing_info_present || vui.num_units_in_tick == 0) {
    return 0.0;
  }
  return static_cast<double>(vui.time_scale) / (2.0 * vui.num_units_in_tick);
}

Sps parse_sps(const uint8_t* data, size_t size) {
  BitReader br(data, size);
  Sps s;
  s.profile_idc = br.read_bits(8);
  br.read_bits(8);  // constraint_set flags + reserved
  s.level_idc = br.read_bits(8);
  s.sps_id = br.read_ue();
  if (!profile_supported(s.profile_idc)) {
    fail_unsupported("profile_idc " + std::to_string(s.profile_idc) + " not supported");
  }
  if (profile_has_extended_format(s.profile_idc)) {
    s.chroma_format_idc = br.read_ue();
    if (s.chroma_format_idc == 3) {
      if (br.read_bit()) {
        fail_unsupported("separate_colour_plane_flag not supported");
      }
    }
    s.bit_depth_luma_minus8 = br.read_ue();
    s.bit_depth_chroma_minus8 = br.read_ue();
    br.read_bit();  // qpprime_y_zero_transform_bypass_flag
    if (br.read_bit()) {  // seq_scaling_matrix_present_flag
      int count = (s.chroma_format_idc != 3) ? 8 : 12;
      for (int i = 0; i < count; ++i) {
        if (br.read_bit()) {
          skip_scaling_list(br, i < 6 ? 16 : 64);
        }
      }
    }
  }
  s.log2_max_frame_num = br.read_ue() + 4;
  s.pic_order_cnt_type = br.read_ue();
  if (s.pic_order_cnt_type == 0) {
    s.log2_max_pic_order_cnt_lsb = br.read_ue() + 4;
  } else if (s.pic_order_cnt_type == 1) {
    s.delta_pic_order_always_zero = br.read_bit() != 0;
    s.offset_for_non_ref_pic = br.read_se();
    s.offset_for_top_to_bottom_field = br.read_se();
    uint32_t cycles = br.read_ue();
    for (uint32_t i = 0; i < cycles; ++i) {
      br.read_se();
    }
  } else if (s.pic_order_cnt_type != 2) {
    fail_parse("pic_order_cnt_type out of range");
  }
  br.read_ue();  // max_num_ref_frames
  br.read_bit();  // gaps_in_frame_num_value_allowed_flag
  s.pic_width_in_mbs = br.read_ue() + 1;
  s.pic_height_in_map_units = br.read_ue() + 1;
  s.frame_mbs_only_flag = br.read_bit() != 0;
  if (!s.frame_mbs_only_flag) {
    s.mb_adaptive_frame_field_flag = br.read_bit() != 0;
  }
  br.read_bit();  // direct_8x8_inference_flag
  s.frame_cropping_flag = br.read_bit() != 0;
  if (s.frame_cropping_flag) {
    s.crop_left = br.read_ue();
    s.crop_right = br.read_ue();
    s.crop_top = br.read_ue();
    s.crop_bottom = br.read_ue();
  }
  if (br.read_bit()) {  // vui_parameters_present_flag
    parse_vui(br, s.vui);
  }
  return s;
}

const Sps* SpsStore::find(uint32_t id) const {
  auto it = map_.find(id);
  return it == map_.end() ? nullptr : &it->second;
}

}  // namespace h264
