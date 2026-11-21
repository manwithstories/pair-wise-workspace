#include "relay/media_clock.hpp"

#include <algorithm>
#include <vector>

namespace relay {

bool is_known_time_base(const TimeBase& tb) {
  static const TimeBase kKnown[] = {
      {1, 90000}, {1, 1000},  {1, 25},     {1, 30},    {1, 30000},
      {1, 50},    {1, 60},    {1, 60000},  {1, 44100}, {1, 48000},
      {1001, 30000}, {1001, 60000},
  };
  if (tb.num <= 0 || tb.den <= 0) return false;
  for (const auto& k : kKnown)
    if (k == tb) return true;
  return false;
}

std::string to_string(const TimeBase& tb) {
  return std::to_string(tb.num) + "/" + std::to_string(tb.den);
}

const char* to_string(ClockError e) {
  switch (e) {
  case ClockError::None: return "none";
  case ClockError::UnknownTimeBase: return "unknown_time_base";
  case ClockError::Duplicate: return "duplicate";
  case ClockError::Backward: return "backward";
  case ClockError::DriftExceeded: return "drift_exceeded";
  }
  return "unknown";
}

ClockRepairer::ClockRepairer(ClockRepairConfig cfg) : cfg_(cfg) {}

RepairedClock ClockRepairer::observe(int64_t node_clock, const TimeBase& tb) {
  RepairedClock r;
  r.raw = node_clock;
  if (!is_known_time_base(tb)) {
    r.unwrapped = node_clock;
    r.repaired = node_clock;
    r.error = ClockError::UnknownTimeBase;
    return r;
  }

  int64_t candidate = node_clock + wrap_offset_;
  // Wrap repair: a drop of more than half the modulus means the counter
  // wrapped around; shift the unwrap offset and keep the timeline moving.
  if (cfg_.wrap_modulus > 0 && has_prev_ &&
      candidate < prev_unwrapped_ - cfg_.wrap_modulus / 2) {
    wrap_offset_ += cfg_.wrap_modulus;
    candidate = node_clock + wrap_offset_;
    ++repaired_wraps_;
  }
  r.unwrapped = candidate;

  if (!has_prev_) {
    has_prev_ = true;
    prev_unwrapped_ = candidate;
    r.repaired = candidate;
    return r;
  }

  if (candidate == prev_unwrapped_) {
    r.error = ClockError::Duplicate;
    r.repaired = candidate; // original value preserved, state untouched
    return r;
  }
  if (candidate < prev_unwrapped_) {
    r.error = ClockError::Backward;
    r.repaired = candidate;
    return r;
  }

  const int64_t delta = candidate - prev_unwrapped_;
  if (!window_.empty()) {
    std::vector<int64_t> sorted(window_.begin(), window_.end());
    std::nth_element(sorted.begin(), sorted.begin() + sorted.size() / 2,
                     sorted.end());
    const int64_t expected = sorted[sorted.size() / 2];
    if (std::llabs(delta - expected) > cfg_.max_drift_ticks) {
      r.error = ClockError::DriftExceeded;
      r.repaired = candidate;      // keep the observed value
      prev_unwrapped_ = candidate; // accept the level, reject the sample
      return r;
    }
  }

  window_.push_back(delta);
  if (window_.size() > cfg_.window_size) window_.pop_front();
  prev_unwrapped_ = candidate;
  r.repaired = candidate;
  return r;
}

} // namespace relay
