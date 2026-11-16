#pragma once

#include <cstdint>
#include <utility>
#include <vector>

namespace h264 {

struct Pps {
    std::uint32_t id = 0;
    std::uint32_t spsId = 0;
    bool entropy_coding_mode_flag = false;
    bool bottom_field_pic_order_in_frame_present_flag = false;
    std::uint32_t num_slice_groups_minus1 = 0;
    std::uint32_t slice_group_map_type = 0;
    std::vector<std::uint32_t> run_length_minus1;
    std::vector<std::pair<std::uint32_t, std::uint32_t>> top_left_bottom_right;
    bool slice_group_change_direction_flag = false;
    std::uint32_t slice_group_change_rate_minus1 = 0;
    std::uint32_t pic_size_in_map_units_minus1 = 0;
    std::vector<std::uint32_t> slice_group_id;
    std::uint32_t num_ref_idx_l0_default_active_minus1 = 0;
    std::uint32_t num_ref_idx_l1_default_active_minus1 = 0;
    bool weighted_pred_flag = false;
    std::uint32_t weighted_bipred_idc = 0;
    std::int32_t pic_init_qp_minus26 = 0;
    std::int32_t pic_init_qs_minus26 = 0;
    std::int32_t chroma_qp_index_offset = 0;
    bool deblocking_filter_control_present_flag = false;
    bool constrained_intra_pred_flag = false;
    bool redundant_pic_cnt_present_flag = false;

    std::uint32_t num_slice_groups() const { return num_slice_groups_minus1 + 1; }
};

Pps parsePps(const std::vector<std::uint8_t>& rbsp);

}  // namespace h264
