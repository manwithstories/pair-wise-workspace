#include "pps_parser.h"

#include "bitreader.h"
#include "exp_golomb.h"

#include <cstddef>

namespace h264 {

namespace {

unsigned bitsForRange(std::uint32_t range) {
    unsigned bits = 0;
    while ((std::uint32_t{1} << bits) < range) {
        ++bits;
    }
    return bits;
}

}  // namespace

Pps parsePps(const std::vector<std::uint8_t>& rbsp) {
    BitReader reader(rbsp);
    Pps pps;
    const std::size_t begin = static_cast<std::size_t>(reader.bitPosition());
    pps.id = readUe(reader);
    pps.spsId = readUe(reader);
    if (pps.id > 255 || pps.spsId > 255) {
        throw ParseError("PPS or SPS id exceeds 255", begin);
    }
    pps.entropy_coding_mode_flag = reader.readBit();
    pps.bottom_field_pic_order_in_frame_present_flag = reader.readBit();
    pps.num_slice_groups_minus1 = readUe(reader);
    if (pps.num_slice_groups_minus1 > 7) {
        throw ParseError("num_slice_groups_minus1 exceeds 7", begin);
    }

    if (pps.num_slice_groups_minus1 > 0) {
        const std::uint32_t groupCount = pps.num_slice_groups();
        pps.slice_group_map_type = readUe(reader);
        if (pps.slice_group_map_type > 6) {
            throw ParseError("invalid slice_group_map_type", begin);
        }
        if (pps.slice_group_map_type == 0) {
            pps.run_length_minus1.reserve(groupCount);
            for (std::uint32_t i = 0; i < groupCount; ++i) {
                pps.run_length_minus1.push_back(readUe(reader));
            }
        } else if (pps.slice_group_map_type == 2) {
            pps.top_left_bottom_right.reserve(groupCount - 1);
            for (std::uint32_t i = 0; i < groupCount - 1; ++i) {
                const std::uint32_t topLeft = readUe(reader);
                const std::uint32_t bottomRight = readUe(reader);
                pps.top_left_bottom_right.emplace_back(topLeft, bottomRight);
            }
        } else if (pps.slice_group_map_type == 3 ||
                   pps.slice_group_map_type == 4 ||
                   pps.slice_group_map_type == 5) {
            pps.slice_group_change_direction_flag = reader.readBit();
            pps.slice_group_change_rate_minus1 = readUe(reader);
        } else if (pps.slice_group_map_type == 6) {
            pps.pic_size_in_map_units_minus1 = readUe(reader);
            if (pps.pic_size_in_map_units_minus1 > 1000000) {
                throw ParseError("FMO picture size exceeds parser limit", begin);
            }
            const unsigned bits = bitsForRange(groupCount);
            pps.slice_group_id.reserve(pps.pic_size_in_map_units_minus1 + 1);
            for (std::uint32_t i = 0; i <= pps.pic_size_in_map_units_minus1; ++i) {
                pps.slice_group_id.push_back(static_cast<std::uint32_t>(reader.readBits(bits)));
            }
        }
    }

    pps.num_ref_idx_l0_default_active_minus1 = readUe(reader);
    pps.num_ref_idx_l1_default_active_minus1 = readUe(reader);
    pps.weighted_pred_flag = reader.readBit();
    pps.weighted_bipred_idc = static_cast<std::uint32_t>(reader.readBits(2));
    if (pps.weighted_bipred_idc > 2) {
        throw ParseError("invalid weighted_bipred_idc", begin);
    }
    pps.pic_init_qp_minus26 = readSe(reader);
    pps.pic_init_qs_minus26 = readSe(reader);
    pps.chroma_qp_index_offset = readSe(reader);
    pps.deblocking_filter_control_present_flag = reader.readBit();
    pps.constrained_intra_pred_flag = reader.readBit();
    pps.redundant_pic_cnt_present_flag = reader.readBit();
    return pps;
}

}  // namespace h264
