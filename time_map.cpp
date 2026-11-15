#include "time_map.hpp"

#include <algorithm>
#include <cstdlib>
#include <limits>
#include <string>

namespace bake {

namespace {

[[noreturn]] void speedError(const std::string& message, int64_t frame = -1) {
    throw BakeError(ExitCode::invalid_input,
                    message + locationSuffix(frame), frame);
}

uint32_t fractionalProgress(int64_t remainder, int64_t denominator) {
    const __int128 product = static_cast<__int128>(remainder) * 65536;
    return static_cast<uint32_t>(product / denominator);
}

}  // namespace

int64_t floorDiv(int64_t numerator, int64_t denominator) {
    if (denominator <= 0) {
        throw BakeError(ExitCode::invalid_input, "rational denominator must be positive");
    }
    int64_t quotient = numerator / denominator;
    const int64_t remainder = numerator % denominator;
    if (remainder < 0) --quotient;
    return quotient;
}

TimeMap::TimeMap(std::vector<SpeedInterval> intervals, int64_t max_source_frame,
                 bool require_continuous_boundaries)
    : intervals_(std::move(intervals)), max_source_frame_(max_source_frame) {
    if (max_source_frame_ < 0) {
        speedError("source frame range is empty");
    }
    if (intervals_.empty()) {
        speedError("speed map has no intervals");
    }
    int64_t accumulated = 0;
    for (std::size_t index = 0; index < intervals_.size(); ++index) {
        const SpeedInterval& interval = intervals_[index];
        if (interval.output_frames <= 0) {
            speedError("speed interval has no output frames", accumulated);
        }
        if (interval.rate_den <= 0) {
            speedError("speed interval denominator must be positive", accumulated);
        }
        if (interval.source_start < 0 || interval.source_start > max_source_frame_) {
            speedError("speed interval source start is outside the clip", accumulated);
        }
        output_starts_.push_back(accumulated);
        accumulated = checkedAdd(accumulated, interval.output_frames,
                                 "speed-map output length");
        const int64_t last_offset = checkedAdd(
            accumulated, -1, "speed-map frame number");
        const TimePoint last = evaluate(last_offset, output_starts_.back(), interval);
        if (last.source_frame < 0 || last.source_frame > max_source_frame_) {
            speedError("speed interval crosses the source-frame range", last_offset);
        }
        if (require_continuous_boundaries && index + 1 < intervals_.size()) {
            const int64_t advance = checkedMul(interval.output_frames,
                                               interval.rate_num,
                                               "speed boundary integration");
            const int64_t expected_next = checkedAdd(
                interval.source_start, floorDiv(advance, interval.rate_den),
                "speed boundary source frame");
            if (expected_next != intervals_[index + 1].source_start) {
                speedError("speed boundary repeats or skips source frames", accumulated);
            }
        }
    }
    total_frames_ = accumulated;
}

TimePoint TimeMap::evaluate(int64_t offset, int64_t start,
                            const SpeedInterval& interval) const {
    const int64_t local = offset - start;
    const int64_t scaled = checkedMul(local, interval.rate_num,
                                      "speed-map source integration");
    int64_t quotient = scaled / interval.rate_den;
    const int64_t remainder = scaled % interval.rate_den;
    if (remainder < 0) --quotient;
    const int64_t nonnegative_remainder = remainder < 0
                                              ? remainder + interval.rate_den
                                              : remainder;
    const int64_t source = checkedAdd(interval.source_start, quotient,
                                      "speed-map source frame");
    if (source < 0 || source > max_source_frame_) {
        speedError("speed interval source frame is out of range", offset);
    }
    return TimePoint{source, fractionalProgress(nonnegative_remainder,
                                                 interval.rate_den)};
}

TimePoint TimeMap::map(int64_t output_offset) const {
    if (output_offset < 0 || output_offset >= total_frames_) {
        speedError("time-map output frame is out of range", output_offset);
    }
    auto it = std::upper_bound(output_starts_.begin(), output_starts_.end(), output_offset);
    if (it == output_starts_.begin()) {
        speedError("time-map interval lookup failed", output_offset);
    }
    const std::size_t index = static_cast<std::size_t>(it - output_starts_.begin() - 1);
    return evaluate(output_offset, output_starts_[index], intervals_[index]);
}

}  // namespace bake
