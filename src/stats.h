#pragma once

#include "access_unit.h"
#include "common.h"
#include "gop_index.h"

#include <cstddef>
#include <cstdint>
#include <map>
#include <string>
#include <vector>

namespace h264 {

struct StatsSummary {
    std::size_t total_frames = 0;
    std::size_t i_frames = 0;
    std::size_t p_frames = 0;
    std::size_t b_frames = 0;
    std::size_t other_frames = 0;
    std::size_t slices_per_au_max = 0;
    std::size_t total_slices = 0;
    std::map<std::size_t, std::size_t> slices_histogram;
    std::uint64_t total_bytes = 0;
    double average_gop_bytes = 0.0;
    std::map<std::string, std::size_t> warning_counts;
};

StatsSummary compute_stats(const std::vector<AccessUnit>& aus,
                           const std::vector<AccessUnit>& frames,
                           const std::vector<GopInfo>& gops,
                           const std::vector<Warning>& warnings);

}  // namespace h264
