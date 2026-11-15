#pragma once

#include "common.h"
#include "param_sets.h"

#include <cstddef>
#include <cstdint>
#include <string>

namespace h264 {

struct SliceInfo {
    std::size_t nal_index = 0;
    std::uint64_t nal_offset = 0;
    std::uint64_t start_code_offset = 0;
    std::uint8_t nal_ref_idc = 0;
    bool idr = false;
    std::uint32_t first_mb_in_slice = 0;
    int slice_type = 0;
    int slice_type_raw = 0;
    std::uint32_t pps_id = 0;
    std::uint32_t sps_id = 0;
    std::uint32_t frame_num = 0;
    bool field_pic_flag = false;
    bool bottom_field_flag = false;
    std::uint32_t idr_pic_id = 0;
    std::uint32_t pic_order_cnt_lsb = 0;
    std::int64_t delta_pic_order_cnt_bottom = 0;
    std::int64_t delta_pic_order_cnt[2] = {0, 0};
    bool adaptive_ref_pic_marking_mode_flag = false;
    bool no_output_of_prior_pics_flag = false;
    bool long_term_reference_flag = false;
};

bool parse_slice_header(const NalUnit& nal, const std::uint8_t* data, std::size_t size,
                        const ParamSets& params, SliceInfo& out, std::string& error);

}  // namespace h264
