#include "picture_order.h"

#include <limits>

namespace h264 {

void PictureOrderCounter::reset() {
    initialized_ = false;
    prev_frame_num_ = 0;
    max_frame_num_ = 0;
    frame_num_offset_ = 0;
    prev_pic_order_cnt_msb_ = 0;
    prev_pic_order_cnt_lsb_ = 0;
    prev_poc_valid_ = false;
}

bool PictureOrderCounter::compute(const SliceInfo& slice, const SpsInfo& sps,
                                  PocInfo& out, std::string& error) {
    if (sps.log2_max_frame_num == 0 || sps.log2_max_frame_num > 16) {
        error = "invalid log2_max_frame_num";
        return false;
    }
    max_frame_num_ = std::uint32_t{1} << sps.log2_max_frame_num;
    if (slice.frame_num >= max_frame_num_) {
        error = "frame_num exceeds MaxFrameNum";
        return false;
    }
    if (slice.idr) {
        initialized_ = true;
        frame_num_offset_ = 0;
        prev_frame_num_ = slice.frame_num;
        prev_pic_order_cnt_msb_ = 0;
        prev_pic_order_cnt_lsb_ = 0;
        prev_poc_valid_ = true;
        out = PocInfo{};
        out.frame_num_unwrapped = slice.frame_num;
        out.reference_reset = true;
        return true;
    }
    bool wrapped = false;
    if (initialized_ && slice.frame_num < prev_frame_num_) {
        frame_num_offset_ += max_frame_num_;
        wrapped = true;
    }
    const std::uint64_t abs_frame_num = frame_num_offset_ + slice.frame_num;
    if (abs_frame_num > static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max() / 4)) {
        error = "frame_num order counter overflow";
        return false;
    }
    bool ok = false;
    switch (sps.pic_order_cnt_type) {
        case 0: ok = compute_type0(slice, sps, out, error); break;
        case 1: ok = compute_type1(slice, sps, out, error); break;
        case 2: ok = compute_type2(slice, sps, out, error); break;
        default: error = "unsupported pic_order_cnt_type"; return false;
    }
    if (!ok) {
        return false;
    }
    out.frame_num_unwrapped = abs_frame_num;
    out.frame_num_wrapped = wrapped;
    out.reference_reset = false;
    initialized_ = true;
    prev_frame_num_ = slice.frame_num;
    if (slice.nal_ref_idc != 0 && sps.pic_order_cnt_type == 0) {
        prev_pic_order_cnt_msb_ = out.top - static_cast<std::int64_t>(slice.pic_order_cnt_lsb);
        prev_pic_order_cnt_lsb_ = slice.pic_order_cnt_lsb;
    }
    return true;
}

bool PictureOrderCounter::compute_type0(const SliceInfo& slice, const SpsInfo& sps,
                                        PocInfo& out, std::string& error) {
    if (sps.log2_max_pic_order_cnt_lsb == 0 || sps.log2_max_pic_order_cnt_lsb > 16) {
        error = "invalid log2_max_pic_order_cnt_lsb";
        return false;
    }
    const std::uint32_t max_lsb = std::uint32_t{1} << sps.log2_max_pic_order_cnt_lsb;
    std::int64_t msb = 0;
    if (prev_poc_valid_) {
        if (slice.pic_order_cnt_lsb < prev_pic_order_cnt_lsb_ &&
            prev_pic_order_cnt_lsb_ - slice.pic_order_cnt_lsb >= max_lsb / 2U) {
            msb = prev_pic_order_cnt_msb_ + max_lsb;
        } else if (slice.pic_order_cnt_lsb > prev_pic_order_cnt_lsb_ &&
                   slice.pic_order_cnt_lsb - prev_pic_order_cnt_lsb_ > max_lsb / 2U) {
            msb = prev_pic_order_cnt_msb_ - max_lsb;
        } else {
            msb = prev_pic_order_cnt_msb_;
        }
    }
    out.top = msb + static_cast<std::int64_t>(slice.pic_order_cnt_lsb);
    out.bottom = out.top;
    if (!slice.field_pic_flag) {
        out.bottom = out.top + slice.delta_pic_order_cnt_bottom;
    } else if (slice.bottom_field_flag) {
        out.bottom = out.top;
    }
    out.poc = out.top < out.bottom ? out.top : out.bottom;
    return true;
}

bool PictureOrderCounter::compute_type1(const SliceInfo& slice, const SpsInfo& sps,
                                        PocInfo& out, std::string& error) {
    if (slice.idr) {
        out.top = 0;
        out.bottom = 0;
        out.poc = 0;
        return true;
    }
    std::uint64_t abs_frame_num = frame_num_offset_ + slice.frame_num;
    if (sps.offset_for_ref_frame.empty()) {
        abs_frame_num = 0;
    } else if (slice.nal_ref_idc == 0 && abs_frame_num > 0) {
        --abs_frame_num;
    }
    std::int64_t expected = 0;
    if (abs_frame_num > 0) {
        const std::uint64_t cycle = sps.offset_for_ref_frame.size();
        std::int64_t cycle_sum = 0;
        for (const std::int64_t value : sps.offset_for_ref_frame) {
            cycle_sum += value;
        }
        const std::uint64_t full_cycles = (abs_frame_num - 1U) / cycle;
        if (full_cycles > static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max() / 4)) {
            error = "POC type 1 cycle count overflow";
            return false;
        }
        expected = static_cast<std::int64_t>(full_cycles) * cycle_sum;
        const std::uint64_t remainder = (abs_frame_num - 1U) % cycle;
        for (std::uint64_t i = 0; i <= remainder; ++i) {
            expected += sps.offset_for_ref_frame[static_cast<std::size_t>(i)];
        }
    }
    if (slice.nal_ref_idc == 0) {
        expected += sps.offset_for_non_ref_pic;
    }
    out.top = expected + slice.delta_pic_order_cnt[0];
    out.bottom = out.top + sps.offset_for_top_to_bottom_field + slice.delta_pic_order_cnt[1];
    if (slice.field_pic_flag) {
        out.poc = slice.bottom_field_flag ? out.bottom : out.top;
    } else {
        out.poc = out.top < out.bottom ? out.top : out.bottom;
    }
    return true;
}

bool PictureOrderCounter::compute_type2(const SliceInfo& slice, const SpsInfo&,
                                        PocInfo& out, std::string& error) {
    if (slice.idr) {
        out.top = 0;
        out.bottom = 0;
        out.poc = 0;
        return true;
    }
    const std::uint64_t abs_frame_num = frame_num_offset_ + slice.frame_num;
    if (abs_frame_num > static_cast<std::uint64_t>(std::numeric_limits<std::int64_t>::max() / 2)) {
        error = "POC type 2 value overflow";
        return false;
    }
    out.poc = static_cast<std::int64_t>(abs_frame_num) * 2 -
              (slice.nal_ref_idc == 0 ? 1 : 0);
    out.top = out.poc;
    out.bottom = out.poc;
    return true;
}

}  // namespace h264
