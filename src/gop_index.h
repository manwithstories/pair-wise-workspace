#pragma once

#include "access_unit.h"
#include "common.h"

#include <cstddef>
#include <cstdint>
#include <string>
#include <vector>

namespace h264 {

struct GopInfo {
    std::size_t index = 0;
    std::uint64_t start_offset = 0;
    std::uint64_t end_offset = 0;
    std::uint64_t byte_length = 0;
    std::size_t first_nal = 0;
    std::size_t last_nal = 0;
    std::size_t frame_count = 0;
    std::string frame_types;
    std::vector<std::size_t> frame_indices;
    bool starts_with_recovery_point = false;
};

bool build_gops(const std::vector<NalUnit>& nals,
                const std::vector<AccessUnit>& frames,
                std::uint64_t file_size,
                bool use_recovery_points,
                std::size_t min_gop_frames,
                std::vector<GopInfo>& gops,
                std::string& error);

}  // namespace h264
