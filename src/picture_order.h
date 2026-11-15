#pragma once

#include "param_sets.h"
#include "slice_header.h"

#include <cstdint>
#include <string>

namespace h264 {

struct PocInfo {
    std::int64_t top = 0;
    std::int64_t bottom = 0;
    std::int64_t poc = 0;
    std::uint64_t frame_num_unwrapped = 0;
    bool frame_num_wrapped = false;
    bool reference_reset = false;
};

class PictureOrderCounter {
public:
    bool compute(const SliceInfo& slice, const SpsInfo& sps, PocInfo& out, std::string& error);
    void reset();

private:
    bool compute_type0(const SliceInfo& slice, const SpsInfo& sps, PocInfo& out, std::string& error);
    bool compute_type1(const SliceInfo& slice, const SpsInfo& sps, PocInfo& out, std::string& error);
    bool compute_type2(const SliceInfo& slice, const SpsInfo& sps, PocInfo& out, std::string& error);

    bool initialized_ = false;
    std::uint32_t prev_frame_num_ = 0;
    std::uint32_t max_frame_num_ = 0;
    std::uint64_t frame_num_offset_ = 0;
    std::int64_t prev_pic_order_cnt_msb_ = 0;
    std::uint32_t prev_pic_order_cnt_lsb_ = 0;
    bool prev_poc_valid_ = false;
};

}  // namespace h264
