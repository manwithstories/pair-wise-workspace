#include "sps_parser.h"

#include "bitreader.h"
#include "exp_golomb.h"

#include <algorithm>
#include <array>
#include <limits>
#include <sstream>

namespace h264 {

namespace {

constexpr std::array<std::uint32_t, 12> kHighProfiles = {
    100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134};

void skipScalingList(BitReader& reader, unsigned size) {
    std::int32_t lastScale = 8;
    std::int32_t nextScale = 8;
    for (unsigned j = 0; j < size; ++j) {
        if (nextScale != 0) {
            const std::int32_t deltaScale = readSe(reader);
            nextScale = (lastScale + deltaScale + 256) % 256;
        }
        lastScale = nextScale == 0 ? lastScale : nextScale;
    }
}

}  // namespace

bool isHighProfile(std::uint32_t profileIdc) {
    return std::find(kHighProfiles.begin(), kHighProfiles.end(), profileIdc) !=
           kHighProfiles.end();
}

std::uint32_t Sps::width() const {
    const std::uint64_t coded = (static_cast<std::uint64_t>(pic_width_in_mbs_minus1) + 1) * 16;
    const std::uint64_t unit = (chroma_format_idc == 1 || chroma_format_idc == 2) ? 2 : 1;
    const std::uint64_t crop = (static_cast<std::uint64_t>(frame_crop_left_offset) +
                                frame_crop_right_offset) * unit;
    return crop >= coded ? 0U : static_cast<std::uint32_t>(coded - crop);
}

std::uint32_t Sps::height() const {
    const std::uint64_t mapUnits = static_cast<std::uint64_t>(pic_height_in_map_units_minus1) + 1;
    const std::uint64_t coded = mapUnits * (frame_mbs_only_flag ? 16 : 32);
    std::uint64_t unit = frame_mbs_only_flag ? 1 : 2;
    if (chroma_format_idc == 1) {
        unit = static_cast<std::uint64_t>(2 * (2 - (frame_mbs_only_flag ? 1 : 0)));
    } else {
        unit = frame_mbs_only_flag ? 1 : 2;
        if (chroma_format_idc == 2) {
            unit = frame_mbs_only_flag ? 1 : 1;
        }
    }
    const std::uint64_t crop = (static_cast<std::uint64_t>(frame_crop_top_offset) +
                                frame_crop_bottom_offset) * unit;
    return crop >= coded ? 0U : static_cast<std::uint32_t>(coded - crop);
}

Sps parseSps(const std::vector<std::uint8_t>& rbsp) {
    BitReader reader(rbsp);
    Sps sps;
    const std::size_t begin = static_cast<std::size_t>(reader.bitPosition());
    sps.profile_idc = static_cast<std::uint32_t>(reader.readBits(8));
    reader.readBits(8);
    sps.level_idc = static_cast<std::uint32_t>(reader.readBits(8));
    sps.id = readUe(reader);
    if (sps.id > 255) {
        throw ParseError("SPS id exceeds 255", begin);
    }

    if (isHighProfile(sps.profile_idc)) {
        sps.chroma_format_idc = readUe(reader);
        if (sps.chroma_format_idc > 3) {
            throw ParseError("invalid chroma_format_idc", begin);
        }
        if (sps.chroma_format_idc == 3) {
            sps.separate_colour_plane_flag = reader.readBit();
        }
        sps.bit_depth_luma_minus8 = readUe(reader);
        sps.bit_depth_chroma_minus8 = readUe(reader);
        if (sps.bit_depth_luma_minus8 > 6 || sps.bit_depth_chroma_minus8 > 6) {
            throw ParseError("bit depth exceeds 14 bits", begin);
        }
        sps.qpprime_y_zero_transform_bypass_flag = reader.readBit();
        if (reader.readBit()) {
            const unsigned count = sps.chroma_format_idc != 3 ? 8 : 12;
            for (unsigned i = 0; i < count; ++i) {
                if (reader.readBit()) {
                    skipScalingList(reader, i < 6 ? 16 : 64);
                }
            }
        }
    }

    const std::uint32_t log2FrameNumMinus4 = readUe(reader);
    if (log2FrameNumMinus4 > 12) {
        throw ParseError("log2_max_frame_num_minus4 exceeds 12", begin);
    }
    sps.log2_max_frame_num = log2FrameNumMinus4 + 4;
    sps.pic_order_cnt_type = readUe(reader);
    if (sps.pic_order_cnt_type > 2) {
        throw ParseError("invalid pic_order_cnt_type", begin);
    }
    if (sps.pic_order_cnt_type == 0) {
        const std::uint32_t log2PocMinus4 = readUe(reader);
        if (log2PocMinus4 > 12) {
            throw ParseError("log2_max_pic_order_cnt_lsb_minus4 exceeds 12", begin);
        }
        sps.log2_max_pic_order_cnt_lsb = log2PocMinus4 + 4;
    } else if (sps.pic_order_cnt_type == 1) {
        sps.delta_pic_order_always_zero_flag = reader.readBit();
        sps.offset_for_non_ref_pic = readSe(reader);
        sps.offset_for_top_to_bottom_field = readSe(reader);
        sps.num_ref_frames_in_pic_order_cnt_cycle = readUe(reader);
        if (sps.num_ref_frames_in_pic_order_cnt_cycle > 255) {
            throw ParseError("POC cycle length exceeds 255", begin);
        }
        sps.offset_for_ref_frame.reserve(sps.num_ref_frames_in_pic_order_cnt_cycle);
        for (std::uint32_t i = 0; i < sps.num_ref_frames_in_pic_order_cnt_cycle; ++i) {
            sps.offset_for_ref_frame.push_back(readSe(reader));
        }
    }

    sps.max_num_ref_frames = readUe(reader);
    if (sps.max_num_ref_frames > 16) {
        throw ParseError("max_num_ref_frames exceeds 16", begin);
    }
    sps.gaps_in_frame_num_value_allowed_flag = reader.readBit();
    sps.pic_width_in_mbs_minus1 = readUe(reader);
    sps.pic_height_in_map_units_minus1 = readUe(reader);
    if (sps.pic_width_in_mbs_minus1 > 4095 ||
        sps.pic_height_in_map_units_minus1 > 4095) {
        throw ParseError("picture dimensions exceed parser limit", begin);
    }
    sps.frame_mbs_only_flag = reader.readBit();
    if (!sps.frame_mbs_only_flag) {
        sps.mb_adaptive_frame_field_flag = reader.readBit();
    }
    sps.direct_8x8_inference_flag = reader.readBit();
    sps.frame_cropping_flag = reader.readBit();
    if (sps.frame_cropping_flag) {
        sps.frame_crop_left_offset = readUe(reader);
        sps.frame_crop_right_offset = readUe(reader);
        sps.frame_crop_top_offset = readUe(reader);
        sps.frame_crop_bottom_offset = readUe(reader);
    }
    return sps;
}

}  // namespace h264
