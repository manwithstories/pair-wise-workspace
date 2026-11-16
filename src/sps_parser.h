#pragma once

#include <cstdint>
#include <vector>

namespace h264 {

struct Sps {
    std::uint32_t id = 0;
    std::uint32_t profile_idc = 0;
    std::uint32_t level_idc = 0;
    std::uint32_t chroma_format_idc = 1;
    bool separate_colour_plane_flag = false;
    std::uint32_t bit_depth_luma_minus8 = 0;
    std::uint32_t bit_depth_chroma_minus8 = 0;
    bool qpprime_y_zero_transform_bypass_flag = false;

    std::uint32_t log2_max_frame_num = 4;
    std::uint32_t pic_order_cnt_type = 0;
    std::uint32_t log2_max_pic_order_cnt_lsb = 4;
    bool delta_pic_order_always_zero_flag = false;
    std::int32_t offset_for_non_ref_pic = 0;
    std::int32_t offset_for_top_to_bottom_field = 0;
    std::uint32_t num_ref_frames_in_pic_order_cnt_cycle = 0;
    std::vector<std::int32_t> offset_for_ref_frame;

    std::uint32_t max_num_ref_frames = 0;
    bool gaps_in_frame_num_value_allowed_flag = false;
    std::uint32_t pic_width_in_mbs_minus1 = 0;
    std::uint32_t pic_height_in_map_units_minus1 = 0;
    bool frame_mbs_only_flag = true;
    bool mb_adaptive_frame_field_flag = false;
    bool direct_8x8_inference_flag = true;
    bool frame_cropping_flag = false;
    std::uint32_t frame_crop_left_offset = 0;
    std::uint32_t frame_crop_right_offset = 0;
    std::uint32_t frame_crop_top_offset = 0;
    std::uint32_t frame_crop_bottom_offset = 0;

    std::uint32_t width() const;
    std::uint32_t height() const;
};

bool isHighProfile(std::uint32_t profileIdc);
Sps parseSps(const std::vector<std::uint8_t>& rbsp);

}  // namespace h264
