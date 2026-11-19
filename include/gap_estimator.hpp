#pragma once

// Estimation of lost sequence ranges.
//
// The reorder stage only reports which sequence numbers will never arrive. This
// module turns each range into a time interval using the surrounding repaired
// frames: it interpolates by sequence distance so a hole inside a burst spreads
// evenly, and extrapolates from the single available side at stream edges. No
// frame is fabricated.

#include <cstdint>
#include <vector>

#include "media_types.hpp"

namespace mfs {

struct GapEstimateOptions {
  int64_t nominal_interval_us{33333};
};

// Appends one LostRange per gap. Gaps are assumed to be sorted by sequence.
void estimate_lost_ranges(const std::vector<SequenceGap>& gaps,
                          const std::vector<RepairedFrame>& frames,
                          const GapEstimateOptions& options,
                          std::vector<LostRange>& out);

}  // namespace mfs
