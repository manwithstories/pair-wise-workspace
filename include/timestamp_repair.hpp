#pragma once

// Timestamp repair for a sequence-ordered frame stream.
//
// Three classes of defects are handled:
//   * capture-clock wraparound (32-bit clock rolls over mid-stream),
//   * missing timestamps (the capture side flagged them, or the clock is 0),
//   * duplicates / backwards jumps (jitter or re-ordering survivors).
//
// Lost intervals produced by the reorder stage are estimated separately in
// gap_estimator.hpp.
//
// The repaired timeline is strictly increasing and expressed in microseconds.
// The raw capture value is always kept on the frame so callers can compare
// before/after. Because the whole file is available at once, a wraparound can
// be detected from the unwrap candidate that best fits the surrounding frames.

#include <cstdint>
#include <vector>

#include "media_types.hpp"
namespace mfs {

struct TimestampOptions {
  int64_t nominal_interval_us{33333};
  // Maximum per-frame deviation tolerated before a value is treated as drift.
  int64_t drift_tolerance_us{2000};
  // Sanity bound for a wrap epoch: the clock is 32-bit, so one rollover is
  // ~4295 s at 1 MHz. A detected epoch is capped at this many rollovers.
  uint32_t max_wrap_epochs{64};
};

class TimestampRepair {
 public:
  explicit TimestampRepair(TimestampOptions options = {});

  // Repairs frames in-place. Frames must already be in sequence order; the
  // function is idempotent with respect to `original_ts_us`, which is left as
  // the unwrapped raw value (-1 when unavailable).
  TimestampStats repair(std::vector<RepairedFrame>& frames);

 private:
  static bool clock_valid(const RepairedFrame& frame);
  int64_t to_microseconds(const RepairedFrame& frame) const;
  int64_t estimate_missing(size_t index,
                           const std::vector<RepairedFrame>& frames,
                           const std::vector<int64_t>& unwrapped,
                           int64_t nominal, int64_t previous_ts) const;

  TimestampOptions options_;
};

}  // namespace mfs
