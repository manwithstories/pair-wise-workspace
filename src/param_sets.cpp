#include "param_sets.h"

#include "bit_reader.h"

#include <limits>
#include <sstream>

namespace h264 {
namespace {

bool reader_failure(const BitReader& reader, const char* prefix, std::string& error) {
    if (reader.ok()) {
        return false;
    }
    error = std::string(prefix) + ": " + reader.error();
    return true;
}

bool is_high_profile(std::uint32_t profile_idc) {
    switch (profile_idc) {
        case 44: case 83: case 86: case 100: case 110: case 118:
        case 122: case 128: case 134: case 135: case 138: case 139:
        case 244:
            return true;
        default:
            return false;
    }
}

bool parse_scaling_list(BitReader& reader, int size, std::string& error) {
    int last_scale = 8;
    int next_scale = 8;
    for (int j = 0; j < size; ++j) {
        if (next_scale != 0) {
            const std::int64_t delta_scale = reader.se();
            if (reader_failure(reader, "seq_scaling_list delta_scale", error)) {
                return false;
            }
            if (delta_scale < -128 || delta_scale > 127) {
                error = "seq_scaling_list delta_scale out of range [-128,127]";
                return false;
            }
            next_scale = (last_scale + static_cast<int>(delta_scale) + 256) % 256;
        }
        if (next_scale != 0) {
            last_scale = next_scale;
        }
    }
    return true;
}

bool parse_vui(BitReader& reader, SpsInfo& sps, std::string& error) {
    sps.vui_parameters_present_flag = true;
    if (reader.u(1) != 0) {
        const std::uint64_t aspect_ratio_idc = reader.u(8);
        if (aspect_ratio_idc == 255) {
            reader.u(16);
            reader.u(16);
        }
    }
    if (reader.u(1) != 0) {
        reader.u(1);
    }
    if (reader.u(1) != 0) {
        reader.u(3);
        reader.u(1);
        if (reader.u(1) != 0) {
            reader.u(8);
            reader.u(8);
            reader.u(8);
        }
    }
    if (reader.u(1) != 0) {
        reader.ue();
        reader.ue();
    }
    if (reader.u(1) != 0) {
        sps.timing_info_present_flag = true;
        sps.num_units_in_tick = static_cast<std::uint32_t>(reader.u(32));
        sps.time_scale = static_cast<std::uint32_t>(reader.u(32));
        sps.fixed_frame_rate_flag = reader.u(1) != 0;
    }
    if (reader_failure(reader, "vui_parameters", error)) {
        return false;
    }
    return true;
}

}  // namespace

bool parse_sps(const std::uint8_t* nal, std::size_t size, SpsInfo& out, std::string& error) {
    if (nal == nullptr || size < 4) {
        error = "SPS NAL is truncated";
        return false;
    }
    BitReader reader(nal + 1, size - 1);
    out = SpsInfo{};
    out.profile_idc = static_cast<std::uint32_t>(reader.u(8));
    out.constraint_flags = static_cast<std::uint32_t>(reader.u(8));
    out.level_idc = static_cast<std::uint32_t>(reader.u(8));
    out.id = static_cast<std::uint32_t>(reader.ue());
    if (reader_failure(reader, "sps", error)) {
        return false;
    }
    out.high_profile = is_high_profile(out.profile_idc);
    if (out.high_profile) {
        out.chroma_format_idc = static_cast<std::uint32_t>(reader.ue());
        if (out.chroma_format_idc > 3) {
            error = "chroma_format_idc must be in [0,3]";
            return false;
        }
        if (out.chroma_format_idc == 3) {
            out.separate_colour_plane_flag = reader.u(1) != 0;
        }
        const std::uint64_t bit_depth_luma_minus8 = reader.ue();
        const std::uint64_t bit_depth_chroma_minus8 = reader.ue();
        if (bit_depth_luma_minus8 > 6) {
            error = "bit_depth_luma_minus8 must be in [0,6]";
            return false;
        }
        if (bit_depth_chroma_minus8 > 6) {
            error = "bit_depth_chroma_minus8 must be in [0,6]";
            return false;
        }
        out.bit_depth_luma = static_cast<std::uint32_t>(bit_depth_luma_minus8 + 8);
        out.bit_depth_chroma = static_cast<std::uint32_t>(bit_depth_chroma_minus8 + 8);
        reader.u(1);
        if (reader.u(1) != 0) {
            const int lists = out.chroma_format_idc == 3 ? 12 : 8;
            for (int i = 0; i < lists; ++i) {
                if (reader.u(1) != 0 && !parse_scaling_list(reader, i < 6 ? 16 : 64, error)) {
                    return false;
                }
            }
        }
    }
    const std::uint64_t log2_frame_num_minus4 = reader.ue();
    if (log2_frame_num_minus4 > 12) {
        error = "log2_max_frame_num_minus4 must be in [0,12]";
        return false;
    }
    out.log2_max_frame_num = static_cast<std::uint32_t>(log2_frame_num_minus4 + 4);
    out.pic_order_cnt_type = static_cast<std::uint32_t>(reader.ue());
    if (out.pic_order_cnt_type > 2) {
        error = "pic_order_cnt_type must be 0, 1, or 2";
        return false;
    }
    if (out.pic_order_cnt_type == 0) {
        const std::uint64_t lsb_minus4 = reader.ue();
        if (lsb_minus4 > 12) {
            error = "log2_max_pic_order_cnt_lsb_minus4 must be in [0,12]";
            return false;
        }
        out.log2_max_pic_order_cnt_lsb = static_cast<std::uint32_t>(lsb_minus4 + 4);
    } else if (out.pic_order_cnt_type == 1) {
        out.delta_pic_order_always_zero_flag = reader.u(1) != 0;
        out.offset_for_non_ref_pic = reader.se();
        out.offset_for_top_to_bottom_field = reader.se();
        const std::uint64_t cycle_length = reader.ue();
        if (cycle_length > 255) {
            error = "num_ref_frames_in_pic_order_cnt_cycle must be in [0,255]";
            return false;
        }
        out.offset_for_ref_frame.reserve(static_cast<std::size_t>(cycle_length));
        for (std::uint64_t i = 0; i < cycle_length; ++i) {
            out.offset_for_ref_frame.push_back(reader.se());
        }
    }
    out.max_num_ref_frames = static_cast<std::uint32_t>(reader.ue());
    out.gaps_in_frame_num_value_allowed_flag = reader.u(1) != 0;
    const std::uint64_t width_mbs_minus1 = reader.ue();
    const std::uint64_t height_map_minus1 = reader.ue();
    out.frame_mbs_only_flag = reader.u(1) != 0;
    if (!out.frame_mbs_only_flag) {
        out.mb_adaptive_frame_field_flag = reader.u(1) != 0;
    }
    out.direct_8x8_inference_flag = reader.u(1) != 0;
    out.frame_cropping_flag = reader.u(1) != 0;
    if (out.frame_cropping_flag) {
        out.crop_left = static_cast<std::uint32_t>(reader.ue());
        out.crop_right = static_cast<std::uint32_t>(reader.ue());
        out.crop_top = static_cast<std::uint32_t>(reader.ue());
        out.crop_bottom = static_cast<std::uint32_t>(reader.ue());
    }
    if (reader.u(1) != 0 && !parse_vui(reader, out, error)) {
        return false;
    }
    if (reader_failure(reader, "sps", error)) {
        return false;
    }
    if (width_mbs_minus1 > 4095) {
        error = "pic_width_in_mbs_minus1 exceeds implementation limit";
        return false;
    }
    if (height_map_minus1 > 4095) {
        error = "pic_height_in_map_units_minus1 exceeds implementation limit";
        return false;
    }
    out.pic_width_in_mbs = static_cast<std::uint32_t>(width_mbs_minus1 + 1);
    out.pic_height_in_map_units = static_cast<std::uint32_t>(height_map_minus1 + 1);
    out.coded_width = out.pic_width_in_mbs * 16U;
    out.coded_height = out.pic_height_in_map_units * 16U *
                       (out.frame_mbs_only_flag ? 1U : 2U);
    std::uint32_t crop_unit_x = 1;
    std::uint32_t crop_unit_y = out.frame_mbs_only_flag ? 1U : 2U;
    if (out.chroma_format_idc == 1 || out.chroma_format_idc == 2) {
        crop_unit_x = 2;
    }
    if (out.chroma_format_idc == 1) {
        crop_unit_y *= 2U;
    }
    const std::uint64_t crop_x = static_cast<std::uint64_t>(out.crop_left + out.crop_right) * crop_unit_x;
    const std::uint64_t crop_y = static_cast<std::uint64_t>(out.crop_top + out.crop_bottom) * crop_unit_y;
    if (crop_x >= out.coded_width) {
        error = "frame_crop_left_offset + frame_crop_right_offset exceeds coded width";
        return false;
    }
    if (crop_y >= out.coded_height) {
        error = "frame_crop_top_offset + frame_crop_bottom_offset exceeds coded height";
        return false;
    }
    out.width = static_cast<std::uint32_t>(out.coded_width - crop_x);
    out.height = static_cast<std::uint32_t>(out.coded_height - crop_y);
    return true;
}

bool parse_pps(const std::uint8_t* nal, std::size_t size, PpsInfo& out, std::string& error) {
    if (nal == nullptr || size < 2) {
        error = "PPS NAL is truncated";
        return false;
    }
    BitReader reader(nal + 1, size - 1);
    out = PpsInfo{};
    out.id = static_cast<std::uint32_t>(reader.ue());
    out.sps_id = static_cast<std::uint32_t>(reader.ue());
    out.entropy_coding_mode_flag = reader.u(1) != 0;
    out.bottom_field_pic_order_in_frame_present_flag = reader.u(1) != 0;
    const std::uint64_t num_slice_groups_minus1 = reader.ue();
    if (num_slice_groups_minus1 > 7) {
        error = "num_slice_groups_minus1 must be in [0,7]";
        return false;
    }
    out.num_slice_groups_minus1 = static_cast<std::uint32_t>(num_slice_groups_minus1);
    if (out.num_slice_groups_minus1 > 0) {
        const std::uint64_t map_type = reader.ue();
        if (map_type > 6) {
            error = "slice_group_map_type must be in [0,6]";
            return false;
        }
        if (map_type == 0) {
            for (std::uint32_t i = 0; i <= out.num_slice_groups_minus1; ++i) {
                reader.ue();
            }
        } else if (map_type == 2) {
            for (std::uint32_t i = 0; i <= out.num_slice_groups_minus1; ++i) {
                reader.ue();
                reader.ue();
            }
        } else if (map_type >= 3 && map_type <= 5) {
            reader.u(1);
            reader.ue();
        } else if (map_type == 6) {
            const std::uint64_t map_units = reader.ue();
            if (map_units > (std::uint64_t{1} << 20U)) {
                error = "pic_size_in_map_units_minus1 exceeds implementation limit";
                return false;
            }
            int bits = 0;
            while ((std::uint64_t{1} << bits) < static_cast<std::uint64_t>(out.num_slice_groups_minus1 + 1)) {
                ++bits;
            }
            for (std::uint64_t i = 0; i <= map_units; ++i) {
                reader.u(bits);
            }
        }
    }
    const std::uint64_t ref_idx_l0 = reader.ue();
    const std::uint64_t ref_idx_l1 = reader.ue();
    if (ref_idx_l0 > 31) {
        error = "num_ref_idx_l0_default_active_minus1 must be in [0,31]";
        return false;
    }
    if (ref_idx_l1 > 31) {
        error = "num_ref_idx_l1_default_active_minus1 must be in [0,31]";
        return false;
    }
    out.num_ref_idx_l0_default_active_minus1 = static_cast<std::uint32_t>(ref_idx_l0);
    out.num_ref_idx_l1_default_active_minus1 = static_cast<std::uint32_t>(ref_idx_l1);
    out.weighted_pred_flag = reader.u(1) != 0;
    const std::uint64_t weighted_bipred_idc = reader.u(2);
    if (weighted_bipred_idc > 2) {
        error = "weighted_bipred_idc must be in [0,2]";
        return false;
    }
    out.weighted_bipred_idc = static_cast<std::uint32_t>(weighted_bipred_idc);
    reader.se();
    reader.se();
    reader.se();
    reader.u(1);
    reader.u(1);
    out.redundant_pic_cnt_present_flag = reader.u(1) != 0;
    if (reader_failure(reader, "pps", error)) {
        return false;
    }
    return true;
}

}  // namespace h264
