#include "h264_headers.h"

#include "bit_reader.h"

#include <cstddef>
#include <limits>

namespace h264scan {
namespace {

bool isHighProfile(uint32_t profile_idc) {
    switch (profile_idc) {
        case 44:
        case 83:
        case 86:
        case 100:
        case 110:
        case 118:
        case 122:
        case 128:
        case 134:
        case 135:
        case 138:
        case 139:
        case 244:
            return true;
        default:
            return false;
    }
}

bool extractRbsp(const std::vector<uint8_t>& nal, uint8_t expected_type,
                 std::vector<uint8_t>& rbsp, std::string& error) {
    if (nal.empty()) {
        error = "empty NAL unit";
        return false;
    }
    const uint8_t type = static_cast<uint8_t>(nal.front() & 0x1fU);
    if (type != expected_type) {
        error = "unexpected NAL type";
        return false;
    }
    const std::vector<uint8_t> ebsp(nal.begin() + 1, nal.end());
    if (!BitReader::removeEmulationPrevention(ebsp, rbsp, error)) {
        return false;
    }
    return true;
}

bool parseScalingList(BitReader& reader, unsigned int size, std::string& error) {
    int32_t last_scale = 8;
    int32_t next_scale = 8;
    for (unsigned int i = 0; i < size; ++i) {
        int32_t delta_scale = 0;
        if (next_scale != 0 && !reader.readSE(delta_scale)) {
            error = reader.error();
            return false;
        }
        if (next_scale != 0) {
            next_scale = (last_scale + delta_scale + 256) % 256;
        }
        if (next_scale != 0) {
            last_scale = next_scale;
        }
    }
    return true;
}

bool readUnsigned(BitReader& reader, unsigned int bits, uint32_t& value,
                  std::string& error) {
    if (!reader.readBits(bits, value)) {
        error = reader.error();
        return false;
    }
    return true;
}

}  // namespace

bool parseSpsNal(const std::vector<uint8_t>& nal, Sps& sps, std::string& error) {
    error.clear();
    std::vector<uint8_t> rbsp;
    if (!extractRbsp(nal, 7U, rbsp, error)) {
        return false;
    }

    BitReader reader(rbsp);
    Sps parsed;
    uint32_t flags = 0;
    if (!readUnsigned(reader, 8U, parsed.profile_idc, error) ||
        !readUnsigned(reader, 8U, flags, error) ||
        !readUnsigned(reader, 8U, parsed.level_idc, error) ||
        !reader.readUE(parsed.id)) {
        if (error.empty()) error = reader.error();
        return false;
    }
    parsed.constraint_flags = flags;

    if (isHighProfile(parsed.profile_idc)) {
        if (!reader.readUE(parsed.chroma_format_idc)) {
            error = reader.error();
            return false;
        }
        if (parsed.chroma_format_idc > 3U) {
            error = "invalid chroma_format_idc";
            return false;
        }
        if (parsed.chroma_format_idc == 3U) {
            bool flag = false;
            if (!reader.readBit(flag)) {
                error = reader.error();
                return false;
            }
            parsed.separate_colour_plane_flag = flag;
        }
        if (!reader.readUE(parsed.bit_depth_luma_minus8) ||
            !reader.readUE(parsed.bit_depth_chroma_minus8)) {
            error = reader.error();
            return false;
        }
        bool bypass = false;
        if (!reader.readBit(bypass)) {
            error = reader.error();
            return false;
        }
        bool scaling_present = false;
        if (!reader.readBit(scaling_present)) {
            error = reader.error();
            return false;
        }
        parsed.seq_scaling_matrix_present_flag = scaling_present;
        if (scaling_present) {
            const unsigned int lists = parsed.chroma_format_idc == 3U ? 12U : 8U;
            for (unsigned int i = 0; i < lists; ++i) {
                bool present = false;
                if (!reader.readBit(present)) {
                    error = reader.error();
                    return false;
                }
                if (present && !parseScalingList(reader, i < 6U ? 16U : 64U, error)) {
                    return false;
                }
            }
        }
    }

    if (!reader.readUE(parsed.log2_max_frame_num_minus4) ||
        !reader.readUE(parsed.pic_order_cnt_type)) {
        error = reader.error();
        return false;
    }
    if (parsed.log2_max_frame_num_minus4 > 12U) {
        error = "log2_max_frame_num_minus4 exceeds limit";
        return false;
    }
    if (parsed.pic_order_cnt_type > 2U) {
        error = "unsupported pic_order_cnt_type";
        return false;
    }

    if (parsed.pic_order_cnt_type == 0U) {
        if (!reader.readUE(parsed.log2_max_pic_order_cnt_lsb_minus4)) {
            error = reader.error();
            return false;
        }
        if (parsed.log2_max_pic_order_cnt_lsb_minus4 > 12U) {
            error = "log2_max_pic_order_cnt_lsb_minus4 exceeds limit";
            return false;
        }
    } else if (parsed.pic_order_cnt_type == 1U) {
        bool always_zero = false;
        if (!reader.readBit(always_zero) ||
            !reader.readSE(parsed.offset_for_non_ref_pic) ||
            !reader.readSE(parsed.offset_for_top_to_bottom_field)) {
            error = reader.error();
            return false;
        }
        parsed.delta_pic_order_always_zero_flag = always_zero;
        uint32_t cycle_length = 0;
        if (!reader.readUE(cycle_length)) {
            error = reader.error();
            return false;
        }
        if (cycle_length > 1024U) {
            error = "POC cycle length exceeds limit";
            return false;
        }
        parsed.offset_for_ref_frame.reserve(cycle_length);
        for (uint32_t i = 0; i < cycle_length; ++i) {
            int32_t offset = 0;
            if (!reader.readSE(offset)) {
                error = reader.error();
                return false;
            }
            parsed.offset_for_ref_frame.push_back(offset);
        }
    }

    bool gaps = false;
    if (!reader.readUE(parsed.max_num_ref_frames) ||
        !reader.readBit(gaps) ||
        !reader.readUE(parsed.pic_width_in_mbs_minus1) ||
        !reader.readUE(parsed.pic_height_in_map_units_minus1)) {
        error = reader.error();
        return false;
    }
    parsed.gaps_in_frame_num_value_allowed_flag = gaps;

    bool frame_mbs_only = false;
    if (!reader.readBit(frame_mbs_only)) {
        error = reader.error();
        return false;
    }
    parsed.frame_mbs_only_flag = frame_mbs_only;
    if (!frame_mbs_only) {
        bool adaptive = false;
        if (!reader.readBit(adaptive)) {
            error = reader.error();
            return false;
        }
        parsed.mb_adaptive_frame_field_flag = adaptive;
    }
    bool direct_8x8 = false;
    if (!reader.readBit(direct_8x8)) {
        error = reader.error();
        return false;
    }
    parsed.direct_8x8_inference_flag = direct_8x8;

    bool cropping = false;
    if (!reader.readBit(cropping)) {
        error = reader.error();
        return false;
    }
    parsed.frame_cropping_flag = cropping;
    if (cropping &&
        (!reader.readUE(parsed.crop_left) || !reader.readUE(parsed.crop_right) ||
         !reader.readUE(parsed.crop_top) || !reader.readUE(parsed.crop_bottom))) {
        error = reader.error();
        return false;
    }

    const uint64_t width_mbs = static_cast<uint64_t>(parsed.pic_width_in_mbs_minus1) + 1U;
    const uint64_t height_units = static_cast<uint64_t>(parsed.pic_height_in_map_units_minus1) + 1U;
    const uint64_t coded_width = width_mbs * 16U;
    const uint64_t coded_height = height_units * (frame_mbs_only ? 16U : 32U);
    if (coded_width > std::numeric_limits<uint32_t>::max() ||
        coded_height > std::numeric_limits<uint32_t>::max()) {
        error = "SPS picture dimensions overflow";
        return false;
    }
    parsed.coded_width = static_cast<uint32_t>(coded_width);
    parsed.coded_height = static_cast<uint32_t>(coded_height);

    uint32_t crop_unit_x = 1;
    uint32_t crop_unit_y = frame_mbs_only ? 1U : 2U;
    if (!parsed.separate_colour_plane_flag && parsed.chroma_format_idc != 0U) {
        const uint32_t sub_width_c = parsed.chroma_format_idc == 3U ? 1U : 2U;
        const uint32_t sub_height_c = parsed.chroma_format_idc == 1U ? 2U : 1U;
        crop_unit_x = sub_width_c;
        crop_unit_y = sub_height_c * (frame_mbs_only ? 1U : 2U);
    }
    const uint64_t crop_x = (static_cast<uint64_t>(parsed.crop_left) + parsed.crop_right) * crop_unit_x;
    const uint64_t crop_y = (static_cast<uint64_t>(parsed.crop_top) + parsed.crop_bottom) * crop_unit_y;
    if (crop_x >= coded_width || crop_y >= coded_height) {
        error = "SPS cropping removes the whole picture";
        return false;
    }
    parsed.width = static_cast<uint32_t>(coded_width - crop_x);
    parsed.height = static_cast<uint32_t>(coded_height - crop_y);

    bool vui_present = false;
    if (!reader.readBit(vui_present)) {
        error = reader.error();
        return false;
    }
    parsed.present = true;
    sps = parsed;
    return true;
}

namespace {

unsigned int ceilLog2(uint32_t value) {
    unsigned int bits = 0;
    while ((uint64_t{1} << bits) < value) {
        ++bits;
    }
    return bits;
}

bool parsePpsSliceGroups(BitReader& reader, uint32_t group_count,
                         std::string& error) {
    uint32_t map_type = 0;
    if (!reader.readUE(map_type) || map_type > 6U) {
        error = map_type > 6U ? "invalid slice_group_map_type" : reader.error();
        return false;
    }

    if (map_type == 0U) {
        for (uint32_t i = 0; i < group_count; ++i) {
            uint32_t ignored = 0;
            if (!reader.readUE(ignored)) {
                error = reader.error();
                return false;
            }
        }
    } else if (map_type == 2U) {
        for (uint32_t i = 0; i + 1U < group_count; ++i) {
            uint32_t ignored = 0;
            if (!reader.readUE(ignored) || !reader.readUE(ignored)) {
                error = reader.error();
                return false;
            }
        }
    } else if (map_type >= 3U && map_type <= 5U) {
        bool ignored_flag = false;
        uint32_t ignored = 0;
        if (!reader.readBit(ignored_flag) || !reader.readUE(ignored)) {
            error = reader.error();
            return false;
        }
    } else if (map_type == 6U) {
        uint32_t map_units_minus1 = 0;
        if (!reader.readUE(map_units_minus1)) {
            error = reader.error();
            return false;
        }
        if (map_units_minus1 > 1000000U) {
            error = "slice group map is unreasonably large";
            return false;
        }
        const unsigned int bits = ceilLog2(group_count);
        uint32_t ignored = 0;
        for (uint32_t i = 0; i <= map_units_minus1; ++i) {
            if (!reader.readBits(bits, ignored)) {
                error = reader.error();
                return false;
            }
        }
    }
    return true;
}

}  // namespace

bool parsePpsNal(const std::vector<uint8_t>& nal, Pps& pps, std::string& error) {
    error.clear();
    std::vector<uint8_t> rbsp;
    if (!extractRbsp(nal, 8U, rbsp, error)) {
        return false;
    }

    BitReader reader(rbsp);
    Pps parsed;
    bool entropy = false;
    bool bottom_field_present = false;
    if (!reader.readUE(parsed.id) || !reader.readUE(parsed.sps_id) ||
        !reader.readBit(entropy) || !reader.readBit(bottom_field_present) ||
        !reader.readUE(parsed.num_slice_groups_minus1)) {
        error = reader.error();
        return false;
    }
    parsed.entropy_coding_mode_flag = entropy;
    parsed.bottom_field_pic_order_in_frame_present_flag = bottom_field_present;
    if (parsed.num_slice_groups_minus1 > 7U) {
        error = "num_slice_groups_minus1 exceeds limit";
        return false;
    }
    if (parsed.num_slice_groups_minus1 > 0U &&
        !parsePpsSliceGroups(reader, parsed.num_slice_groups_minus1 + 1U, error)) {
        return false;
    }

    bool weighted_pred = false;
    bool deblocking = false;
    bool constrained_intra = false;
    bool redundant = false;
    if (!reader.readUE(parsed.num_ref_idx_l0_default_active_minus1) ||
        !reader.readUE(parsed.num_ref_idx_l1_default_active_minus1) ||
        !reader.readBit(weighted_pred) ||
        !readUnsigned(reader, 2U, parsed.weighted_bipred_idc, error) ||
        !reader.readSE(parsed.pic_init_qp_minus26) ||
        !reader.readSE(parsed.pic_init_qs_minus26) ||
        !reader.readSE(parsed.chroma_qp_index_offset) ||
        !reader.readBit(deblocking) || !reader.readBit(constrained_intra) ||
        !reader.readBit(redundant)) {
        if (error.empty()) error = reader.error();
        return false;
    }
    parsed.weighted_pred_flag = weighted_pred;
    parsed.deblocking_filter_control_present_flag = deblocking;
    parsed.constrained_intra_pred_flag = constrained_intra;
    parsed.redundant_pic_cnt_present_flag = redundant;
    if (deblocking) {
        uint32_t disable_idc = 0;
        int32_t offset_a = 0;
        int32_t offset_b = 0;
        if (!reader.readUE(disable_idc)) {
            error = reader.error();
            return false;
        }
        if (disable_idc != 1U && (!reader.readSE(offset_a) || !reader.readSE(offset_b))) {
            error = reader.error();
            return false;
        }
    }

    parsed.present = true;
    pps = parsed;
    return true;
}

bool peekSlicePpsId(const std::vector<uint8_t>& nal, uint32_t& pps_id,
                    std::string& error) {
    error.clear();
    if (nal.empty()) {
        error = "empty slice NAL";
        return false;
    }
    const uint8_t type = static_cast<uint8_t>(nal.front() & 0x1fU);
    if (type != 1U && type != 5U) {
        error = "NAL is not a coded slice";
        return false;
    }
    const std::vector<uint8_t> ebsp(nal.begin() + 1, nal.end());
    std::vector<uint8_t> rbsp;
    if (!BitReader::removeEmulationPrevention(ebsp, rbsp, error)) {
        return false;
    }
    BitReader reader(rbsp);
    uint32_t ignored = 0;
    if (!reader.readUE(ignored) || !reader.readUE(ignored) ||
        !reader.readUE(pps_id)) {
        error = reader.error();
        return false;
    }
    return true;
}

bool parseSliceHeaderNal(const std::vector<uint8_t>& nal, const Sps& sps,
                         const Pps& pps, SliceHeader& slice, std::string& error) {
    error.clear();
    if (nal.empty()) {
        error = "empty slice NAL";
        return false;
    }
    const uint8_t type = static_cast<uint8_t>(nal.front() & 0x1fU);
    if (type != 1U && type != 5U) {
        error = "NAL is not a coded slice";
        return false;
    }
    const std::vector<uint8_t> ebsp(nal.begin() + 1, nal.end());
    std::vector<uint8_t> rbsp;
    if (!BitReader::removeEmulationPrevention(ebsp, rbsp, error)) {
        return false;
    }

    BitReader reader(rbsp);
    SliceHeader parsed;
    parsed.nal_unit_type = type;
    parsed.nal_ref_idc = static_cast<uint8_t>((nal.front() >> 5U) & 0x03U);
    parsed.idr = type == 5U;
    if (!reader.readUE(parsed.first_mb_in_slice) || !reader.readUE(parsed.slice_type) ||
        !reader.readUE(parsed.pps_id)) {
        error = reader.error();
        return false;
    }
    if (parsed.slice_type > 9U) {
        error = "invalid slice_type";
        return false;
    }
    if (!reader.readBits(sps.frame_num_bits(), parsed.frame_num)) {
        error = reader.error();
        return false;
    }
    if (!sps.frame_mbs_only_flag) {
        bool field_pic = false;
        if (!reader.readBit(field_pic)) {
            error = reader.error();
            return false;
        }
        parsed.field_pic_flag = field_pic;
        if (field_pic) {
            bool bottom_field = false;
            if (!reader.readBit(bottom_field)) {
                error = reader.error();
                return false;
            }
            parsed.bottom_field_flag = bottom_field;
        }
    }
    if (parsed.idr && !reader.readUE(parsed.idr_pic_id)) {
        error = reader.error();
        return false;
    }
    if (sps.pic_order_cnt_type == 0U) {
        if (!reader.readBits(sps.pic_order_cnt_lsb_bits(), parsed.pic_order_cnt_lsb)) {
            error = reader.error();
            return false;
        }
        if (pps.bottom_field_pic_order_in_frame_present_flag && !parsed.field_pic_flag &&
            !reader.readSE(parsed.delta_pic_order_cnt_bottom)) {
            error = reader.error();
            return false;
        }
    } else if (sps.pic_order_cnt_type == 1U && !sps.delta_pic_order_always_zero_flag) {
        if (!reader.readSE(parsed.delta_pic_order_cnt[0])) {
            error = reader.error();
            return false;
        }
        if (pps.bottom_field_pic_order_in_frame_present_flag && !parsed.field_pic_flag &&
            !reader.readSE(parsed.delta_pic_order_cnt[1])) {
            error = reader.error();
            return false;
        }
    }

    const uint64_t width_mbs = static_cast<uint64_t>(sps.pic_width_in_mbs_minus1) + 1U;
    const uint64_t map_units = static_cast<uint64_t>(sps.pic_height_in_map_units_minus1) + 1U;
    uint64_t picture_mbs = width_mbs * map_units;
    if (!parsed.field_pic_flag && !sps.frame_mbs_only_flag) {
        picture_mbs *= 2U;
    }
    if (parsed.first_mb_in_slice >= picture_mbs) {
        error = "first_mb_in_slice is outside the picture";
        return false;
    }

    parsed.valid = true;
    slice = parsed;
    return true;
}

}  // namespace h264scan
