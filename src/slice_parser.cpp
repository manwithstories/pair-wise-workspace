#include "slice_parser.h"

#include "bitreader.h"
#include "exp_golomb.h"

#include <cstddef>

namespace h264 {

SlicePrelude parseSlicePrelude(const std::vector<std::uint8_t>& rbsp) {
    BitReader reader(rbsp);
    SlicePrelude prelude;
    prelude.first_mb_in_slice = readUe(reader);
    prelude.slice_type_raw = readUe(reader);
    prelude.pps_id = readUe(reader);
    if (prelude.first_mb_in_slice > 1000000) {
        throw ParseError("first_mb_in_slice exceeds parser limit", 0);
    }
    if (prelude.slice_type_raw > 9) {
        throw ParseError("invalid slice_type", 0);
    }
    if (prelude.pps_id > 255) {
        throw ParseError("PPS id exceeds 255", 0);
    }
    return prelude;
}

SliceInfo parseSlice(const std::vector<std::uint8_t>& rbsp, bool idr,
                     const Sps& sps, const Pps& pps) {
    BitReader reader(rbsp);
    SliceInfo slice;
    slice.idr = idr;
    slice.first_mb_in_slice = readUe(reader);
    slice.slice_type_raw = readUe(reader);
    slice.slice_type = slice.slice_type_raw % 5;
    switch (slice.slice_type) {
        case 0: slice.frame_type = FrameType::P; break;
        case 1: slice.frame_type = FrameType::B; break;
        case 2: slice.frame_type = FrameType::I; break;
        case 3: slice.frame_type = FrameType::SP; break;
        case 4: slice.frame_type = FrameType::SI; break;
        default: throw ParseError("invalid slice_type", 0);
    }
    if (slice.slice_type_raw > 9) {
        throw ParseError("invalid slice_type", 0);
    }
    slice.pps_id = readUe(reader);
    if (slice.pps_id != pps.id) {
        throw ParseError("slice PPS id does not match active PPS", 0);
    }
    if (sps.separate_colour_plane_flag) {
        slice.has_colour_plane_id = true;
        slice.colour_plane_id = static_cast<std::uint32_t>(reader.readBits(2));
        if (slice.colour_plane_id > 2) {
            throw ParseError("invalid colour_plane_id", 0);
        }
    }

    slice.frame_num = static_cast<std::uint32_t>(reader.readBits(sps.log2_max_frame_num));
    if (!sps.frame_mbs_only_flag) {
        slice.field_pic_flag = reader.readBit();
        if (slice.field_pic_flag) {
            slice.bottom_field_flag = reader.readBit();
        }
    }
    if (idr) {
        slice.idr_pic_id = readUe(reader);
    }

    if (sps.pic_order_cnt_type == 0) {
        slice.pic_order_cnt_lsb = static_cast<std::uint32_t>(
            reader.readBits(sps.log2_max_pic_order_cnt_lsb));
        if (pps.bottom_field_pic_order_in_frame_present_flag && !slice.field_pic_flag) {
            slice.has_delta_poc_bottom = true;
            slice.delta_pic_order_cnt_bottom = readSe(reader);
        }
    } else if (sps.pic_order_cnt_type == 1 && !sps.delta_pic_order_always_zero_flag) {
        slice.has_delta_poc = true;
        slice.delta_pic_order_cnt0 = readSe(reader);
        if (pps.bottom_field_pic_order_in_frame_present_flag && !slice.field_pic_flag) {
            slice.delta_pic_order_cnt1 = readSe(reader);
        }
    }
    if (pps.redundant_pic_cnt_present_flag) {
        slice.redundant_pic_cnt = readUe(reader);
    }
    return slice;
}

const char* frameTypeName(FrameType type) {
    switch (type) {
        case FrameType::I: return "I";
        case FrameType::P: return "P";
        case FrameType::B: return "B";
        case FrameType::SP: return "SP";
        case FrameType::SI: return "SI";
        default: return "?";
    }
}

}  // namespace h264
