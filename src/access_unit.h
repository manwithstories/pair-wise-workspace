#pragma once

#include "common.h"
#include "slice_header.h"

#include <cstddef>
#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace h264 {

struct RecoveryPoint {
    bool present = false;
    std::uint32_t recovery_frame_cnt = 0;
    bool exact_match_flag = false;
    bool broken_link_flag = false;
    std::uint32_t changing_slice_group_idc = 0;
};

struct AccessUnit {
    std::size_t first_nal = 0;
    std::size_t last_nal = 0;
    std::uint64_t start_offset = 0;
    std::uint64_t end_offset = 0;
    bool has_aud = false;
    bool idr = false;
    bool recovery_point = false;
    RecoveryPoint recovery;
    std::size_t first_slice = 0;
    std::size_t slice_count = 0;
    int slice_type = 0;
    std::uint32_t frame_num = 0;
    std::uint64_t frame_num_unwrapped = 0;
    bool field_pic_flag = false;
    bool bottom_field_flag = false;
    bool field_pair_merged = false;
    std::int64_t poc = 0;
};

bool parse_recovery_point(const std::uint8_t* data, std::size_t size,
                          RecoveryPoint& out, std::string& error);

bool build_access_units(const std::vector<NalUnit>& nals,
                        const std::vector<SliceInfo>& slices,
                        const std::map<std::size_t, RecoveryPoint>& recovery_points,
                        std::uint64_t file_size,
                        std::vector<AccessUnit>& aus,
                        std::string& error);

bool merge_field_pairs(const std::vector<AccessUnit>& aus,
                       std::vector<AccessUnit>& frames,
                       std::string& error);

}  // namespace h264
