#pragma once

#include <cstdint>
#include <string>
#include <vector>

namespace h264scan {

struct Sps {
    bool present = false;
    uint32_t id = 0;
    uint32_t profile_idc = 0;
    uint32_t constraint_flags = 0;
    uint32_t level_idc = 0;
    uint32_t chroma_format_idc = 1;
    bool separate_colour_plane_flag = false;
    uint32_t bit_depth_luma_minus8 = 0;
    uint32_t bit_depth_chroma_minus8 = 0;
    bool seq_scaling_matrix_present_flag = false;
    uint32_t log2_max_frame_num_minus4 = 0;
    uint32_t pic_order_cnt_type = 0;
    uint32_t log2_max_pic_order_cnt_lsb_minus4 = 0;
    bool delta_pic_order_always_zero_flag = false;
    int32_t offset_for_non_ref_pic = 0;
    int32_t offset_for_top_to_bottom_field = 0;
    std::vector<int32_t> offset_for_ref_frame;
    uint32_t max_num_ref_frames = 0;
    bool gaps_in_frame_num_value_allowed_flag = false;
    uint32_t pic_width_in_mbs_minus1 = 0;
    uint32_t pic_height_in_map_units_minus1 = 0;
    bool frame_mbs_only_flag = true;
    bool mb_adaptive_frame_field_flag = false;
    bool direct_8x8_inference_flag = true;
    bool frame_cropping_flag = false;
    uint32_t crop_left = 0;
    uint32_t crop_right = 0;
    uint32_t crop_top = 0;
    uint32_t crop_bottom = 0;
    uint32_t coded_width = 0;
    uint32_t coded_height = 0;
    uint32_t width = 0;
    uint32_t height = 0;

    uint32_t frame_num_bits() const { return log2_max_frame_num_minus4 + 4U; }
    uint32_t pic_order_cnt_lsb_bits() const {
        return log2_max_pic_order_cnt_lsb_minus4 + 4U;
    }
    uint32_t macroblocks_in_frame() const {
        return (pic_width_in_mbs_minus1 + 1U) *
               (pic_height_in_map_units_minus1 + 1U) *
               (frame_mbs_only_flag ? 1U : 2U);
    }
};

struct Pps {
    bool present = false;
    uint32_t id = 0;
    uint32_t sps_id = 0;
    bool entropy_coding_mode_flag = false;
    bool bottom_field_pic_order_in_frame_present_flag = false;
    uint32_t num_slice_groups_minus1 = 0;
    uint32_t num_ref_idx_l0_default_active_minus1 = 0;
    uint32_t num_ref_idx_l1_default_active_minus1 = 0;
    bool weighted_pred_flag = false;
    uint32_t weighted_bipred_idc = 0;
    int32_t pic_init_qp_minus26 = 0;
    int32_t pic_init_qs_minus26 = 0;
    int32_t chroma_qp_index_offset = 0;
    bool deblocking_filter_control_present_flag = false;
    bool constrained_intra_pred_flag = false;
    bool redundant_pic_cnt_present_flag = false;
};

struct SliceHeader {
    bool valid = false;
    bool idr = false;
    uint8_t nal_ref_idc = 0;
    uint8_t nal_unit_type = 0;
    uint32_t first_mb_in_slice = 0;
    uint32_t slice_type = 0;
    uint32_t pps_id = 0;
    uint32_t frame_num = 0;
    bool field_pic_flag = false;
    bool bottom_field_flag = false;
    uint32_t idr_pic_id = 0;
    uint32_t pic_order_cnt_lsb = 0;
    int32_t delta_pic_order_cnt_bottom = 0;
    int32_t delta_pic_order_cnt[2] = {0, 0};

    uint32_t normalized_slice_type() const { return slice_type % 5U; }
};

bool parseSpsNal(const std::vector<uint8_t>& nal, Sps& sps, std::string& error);
bool parsePpsNal(const std::vector<uint8_t>& nal, Pps& pps, std::string& error);
bool peekSlicePpsId(const std::vector<uint8_t>& nal, uint32_t& pps_id,
                    std::string& error);
bool parseSliceHeaderNal(const std::vector<uint8_t>& nal, const Sps& sps,
                         const Pps& pps, SliceHeader& slice, std::string& error);

}  // namespace h264scan
