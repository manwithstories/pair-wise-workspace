#pragma once

#include <cstddef>
#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace h264 {

struct SpsInfo {
    std::uint32_t id = 0;
    std::uint32_t profile_idc = 0;
    std::uint32_t constraint_flags = 0;
    std::uint32_t level_idc = 0;
    bool high_profile = false;
    std::uint32_t chroma_format_idc = 1;
    bool separate_colour_plane_flag = false;
    std::uint32_t bit_depth_luma = 8;
    std::uint32_t bit_depth_chroma = 8;
    std::uint32_t log2_max_frame_num = 4;
    std::uint32_t pic_order_cnt_type = 0;
    std::uint32_t log2_max_pic_order_cnt_lsb = 4;
    bool delta_pic_order_always_zero_flag = false;
    std::int64_t offset_for_non_ref_pic = 0;
    std::int64_t offset_for_top_to_bottom_field = 0;
    std::vector<std::int64_t> offset_for_ref_frame;
    std::uint32_t max_num_ref_frames = 0;
    bool gaps_in_frame_num_value_allowed_flag = false;
    std::uint32_t pic_width_in_mbs = 0;
    std::uint32_t pic_height_in_map_units = 0;
    bool frame_mbs_only_flag = true;
    bool mb_adaptive_frame_field_flag = false;
    bool direct_8x8_inference_flag = false;
    bool frame_cropping_flag = false;
    std::uint32_t crop_left = 0;
    std::uint32_t crop_right = 0;
    std::uint32_t crop_top = 0;
    std::uint32_t crop_bottom = 0;
    std::uint32_t coded_width = 0;
    std::uint32_t coded_height = 0;
    std::uint32_t width = 0;
    std::uint32_t height = 0;
    bool vui_parameters_present_flag = false;
    bool timing_info_present_flag = false;
    std::uint32_t num_units_in_tick = 0;
    std::uint32_t time_scale = 0;
    bool fixed_frame_rate_flag = false;
};

struct PpsInfo {
    std::uint32_t id = 0;
    std::uint32_t sps_id = 0;
    bool entropy_coding_mode_flag = false;
    bool bottom_field_pic_order_in_frame_present_flag = false;
    bool redundant_pic_cnt_present_flag = false;
    std::uint32_t num_slice_groups_minus1 = 0;
    std::uint32_t num_ref_idx_l0_default_active_minus1 = 0;
    std::uint32_t num_ref_idx_l1_default_active_minus1 = 0;
    bool weighted_pred_flag = false;
    std::uint32_t weighted_bipred_idc = 0;
};

struct ParamSets {
    std::map<std::uint32_t, SpsInfo> sps;
    std::map<std::uint32_t, PpsInfo> pps;
};

bool parse_sps(const std::uint8_t* nal, std::size_t size, SpsInfo& out, std::string& error);
bool parse_pps(const std::uint8_t* nal, std::size_t size, PpsInfo& out, std::string& error);

}  // namespace h264
