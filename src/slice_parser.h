#pragma once

#include "pps_parser.h"
#include "sps_parser.h"

#include <cstdint>
#include <vector>

namespace h264 {

enum class FrameType { I, P, B, SP, SI, Unknown };

struct SlicePrelude {
    std::uint32_t first_mb_in_slice = 0;
    std::uint32_t slice_type_raw = 0;
    std::uint32_t pps_id = 0;
};

struct SliceInfo {
    std::uint32_t first_mb_in_slice = 0;
    std::uint32_t slice_type_raw = 0;
    std::uint32_t slice_type = 0;
    FrameType frame_type = FrameType::Unknown;
    std::uint32_t pps_id = 0;
    std::uint32_t frame_num = 0;
    bool idr = false;
    bool field_pic_flag = false;
    bool bottom_field_flag = false;
    bool has_colour_plane_id = false;
    std::uint32_t colour_plane_id = 0;
    std::uint32_t idr_pic_id = 0;
    std::uint32_t pic_order_cnt_lsb = 0;
    bool has_delta_poc_bottom = false;
    std::int32_t delta_pic_order_cnt_bottom = 0;
    bool has_delta_poc = false;
    std::int32_t delta_pic_order_cnt0 = 0;
    std::int32_t delta_pic_order_cnt1 = 0;
    std::uint32_t redundant_pic_cnt = 0;
};

SlicePrelude parseSlicePrelude(const std::vector<std::uint8_t>& rbsp);
SliceInfo parseSlice(const std::vector<std::uint8_t>& rbsp, bool idr,
                     const Sps& sps, const Pps& pps);
const char* frameTypeName(FrameType type);

}  // namespace h264
