#pragma once

#include "error.hpp"

#include <cstdint>
#include <vector>

namespace bake {

struct TimePoint {
    int64_t source_frame = 0;
    uint32_t progress_q16 = 0;
};

struct SpeedInterval {
    int64_t output_frames = 0;
    int64_t source_start = 0;
    int64_t rate_num = 1;
    int64_t rate_den = 1;
};

class TimeMap {
public:
    TimeMap(std::vector<SpeedInterval> intervals, int64_t max_source_frame,
            bool require_continuous_boundaries = true);

    TimePoint map(int64_t output_offset) const;
    int64_t totalFrames() const { return total_frames_; }
    const std::vector<SpeedInterval>& intervals() const { return intervals_; }

private:
    std::vector<SpeedInterval> intervals_;
    std::vector<int64_t> output_starts_;
    int64_t max_source_frame_ = 0;
    int64_t total_frames_ = 0;
    TimePoint evaluate(int64_t offset, int64_t start, const SpeedInterval& interval) const;
};

int64_t floorDiv(int64_t numerator, int64_t denominator);

}  // namespace bake
