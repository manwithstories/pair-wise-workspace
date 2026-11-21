#pragma once

#include <cstddef>
#include <cstdint>
#include <deque>
#include <string>

namespace relay {

// Rational time base shared by clock repair and index building.
struct TimeBase {
  int32_t num = 1;
  int32_t den = 90000;

  double to_seconds(int64_t ticks) const {
    return den > 0 ? static_cast<double>(ticks) * static_cast<double>(num) /
                         static_cast<double>(den)
                   : 0.0;
  }
  int64_t to_ticks(double seconds) const {
    return num > 0
               ? static_cast<int64_t>(seconds * static_cast<double>(den) /
                                      static_cast<double>(num))
               : 0;
  }
  bool operator==(const TimeBase&) const = default;
};

// Only whitelisted time bases are accepted on the wire.
bool is_known_time_base(const TimeBase& tb);
std::string to_string(const TimeBase& tb);

// Error model shared across clock repair, decoding and indexing.
enum class ClockError {
  None = 0,
  UnknownTimeBase,  // time base not in the accepted set
  Duplicate,        // repeated delivery of an already-seen timestamp
  Backward,         // timestamp moved backwards without a wrap explanation
  DriftExceeded,    // observed drift beyond the configured limit
};

const char* to_string(ClockError e);

struct ClockRepairConfig {
  size_t window_size = 32;             // sliding window for drift estimate
  int64_t max_drift_ticks = 5 * 90000; // drift limit in clock ticks
  int64_t wrap_modulus = 0;            // 0 disables wrap repair
};

struct RepairedClock {
  int64_t raw = 0;       // original node clock value (always preserved)
  int64_t unwrapped = 0; // after wrap repair
  int64_t repaired = 0;  // after drift correction (== unwrapped on error)
  ClockError error = ClockError::None;
};

// Sliding-window drift estimator + wrap repairer for one source node.
class ClockRepairer {
public:
  explicit ClockRepairer(ClockRepairConfig cfg);

  // Feed one observation; arrival order is implied by call order.
  RepairedClock observe(int64_t node_clock, const TimeBase& tb);

  uint64_t repaired_wraps() const { return repaired_wraps_; }

private:
  ClockRepairConfig cfg_;
  std::deque<int64_t> window_; // recent unwrapped deltas vs. expectation
  int64_t wrap_offset_ = 0;    // accumulated unwrap offset
  int64_t prev_unwrapped_ = 0;
  bool has_prev_ = false;
  uint64_t repaired_wraps_ = 0;
};

} // namespace relay
