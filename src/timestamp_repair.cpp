#include "timestamp_repair.hpp"

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <vector>

namespace mfs {
namespace {

constexpr int64_t kMicrosPerSecond = 1000000;
constexpr uint64_t kClockPeriod = uint64_t{1} << 32;
int64_t clamp_i64(int64_t value, int64_t low, int64_t high) {
  return std::max(low, std::min(value, high));
}

}  // namespace

TimestampRepair::TimestampRepair(TimestampOptions options) : options_(options) {
  options_.nominal_interval_us =
      std::max<int64_t>(options_.nominal_interval_us, 1);
  options_.drift_tolerance_us = std::max<int64_t>(options_.drift_tolerance_us, 1);
}

bool TimestampRepair::clock_valid(const RepairedFrame& frame) {
  return !frame.clock_missing && frame.raw_clock != 0;
}

int64_t TimestampRepair::to_microseconds(const RepairedFrame& frame) const {
  const int64_t hz = frame.capture_hz > 0 ? frame.capture_hz : 1000000;
  return static_cast<int64_t>(frame.raw_clock) * kMicrosPerSecond / hz;
}

TimestampStats TimestampRepair::repair(std::vector<RepairedFrame>& frames) {
  TimestampStats stats;
  if (frames.empty()) {
    stats.strictly_increasing = true;
    stats.nominal_interval_us = options_.nominal_interval_us;
    return stats;
  }

  // Detect the cadence from the observed valid deltas: this keeps repairs
  // correct for capture rates other than the default.
  std::vector<int64_t> deltas;
  deltas.reserve(frames.size());
  for (size_t i = 1; i < frames.size(); ++i) {
    if (!clock_valid(frames[i]) || !clock_valid(frames[i - 1])) {
      continue;
    }
    const int64_t delta = to_microseconds(frames[i]) - to_microseconds(frames[i - 1]);
    if (delta > 0) {
      deltas.push_back(delta);
    }
  }
  int64_t nominal = options_.nominal_interval_us;
  if (!deltas.empty()) {
    // Robust cadence estimate. Corrupted deltas (duplicates -> 0, backwards
    // jumps -> negative, holes -> multiples of the interval) are excluded:
    // only deltas under 1.5x the shortest observed interval vote, and the
    // vote takes the median. This keeps the estimate at one frame interval.
    const int64_t shortest =
        *std::min_element(deltas.begin(), deltas.end());
    const int64_t upper = std::max<int64_t>(shortest + shortest / 2, 1);
    auto end = std::remove_if(deltas.begin(), deltas.end(),
                              [upper](int64_t delta) { return delta > upper; });
    const size_t count = static_cast<size_t>(end - deltas.begin());
    if (count >= 1) {
      // Lower median: conservative towards the shortest observed interval,
      // so an even split between two cadences does not inflate the step.
      std::nth_element(deltas.begin(), deltas.begin() + (count - 1) / 2,
                       deltas.begin() + count);
      const int64_t observed =
          clamp_i64(deltas[(count - 1) / 2], 1, 10 * kMicrosPerSecond);
      // A jittery stream can bias the median by a few percent. When the
      // observed cadence is close to the configured one, trust the configured
      // value; otherwise follow the observed cadence (non-default capture
      // rates). The 25% band also keeps symmetric jitter from flipping the
      // step between two adjacent values.
      const int64_t configured = options_.nominal_interval_us;
      const int64_t band = std::max<int64_t>(configured / 4, 1);
      nominal = std::llabs(observed - configured) <= band ? configured
                                                          : observed;
    }
  }
  stats.nominal_interval_us = nominal;

  // Pass 1: unwrap the 32-bit capture clock and record the raw value.
  const int64_t period_us =
      static_cast<int64_t>(kClockPeriod) * kMicrosPerSecond /
      (frames.front().capture_hz > 0 ? frames.front().capture_hz : 1000000);
  int64_t previous_unwrapped = 0;
  bool have_previous = false;
  std::vector<int64_t> unwrapped(frames.size(), -1);
  for (size_t i = 0; i < frames.size(); ++i) {
    RepairedFrame& frame = frames[i];
    if (!clock_valid(frame)) {
      frame.original_ts_us = -1;
      continue;
    }
    int64_t value = to_microseconds(frame);
    uint32_t epoch = 0;
    if (have_previous) {
      // Advance whole clock periods until the value lands within half a
      // period of the running timeline. Comparing against the raw value
      // alone would fail right after the rollover, where the wrapped value
      // plus one period sits slightly above the pre-rollover timeline.
      const int64_t half_period = period_us / 2;
      const int64_t target = previous_unwrapped;
      while (value + half_period < target &&
             epoch < options_.max_wrap_epochs) {
        value += period_us;
        ++epoch;
      }
    }
    if (epoch > 0) {
      ++stats.wrap_fixes;
    }
    frame.original_ts_us = value;
    unwrapped[i] = value;
    previous_unwrapped = value;
    have_previous = true;
  }

  // Pass 2: fill missing values and enforce a strictly increasing timeline.
  int64_t previous_ts = 0;
  for (size_t i = 0; i < frames.size(); ++i) {
    RepairedFrame& frame = frames[i];
    const int64_t expected = i == 0 ? 0 : previous_ts + nominal;
    int64_t ts = -1;

    if (unwrapped[i] >= 0) {
      ts = unwrapped[i];
      if (i == 0) {
        frame.ts_us = ts;
        previous_ts = ts;
        continue;
      }
      const int64_t delta = ts - previous_ts;
      // A hole left by dropped frames shows up as a delta of at least two
      // intervals. Such deltas are genuine timeline information and must be
      // preserved; only near-cadence jitter is snapped.
      const int64_t hole_threshold =
          nominal + std::max<int64_t>(options_.drift_tolerance_us * 2,
                                      nominal / 2);
      if (delta <= 0) {
        // Duplicate or backwards jump: pin to the expected slot.
        if (delta == 0) {
          ++stats.duplicate_timestamps;
        } else {
          ++stats.backward_timestamps;
        }
        ts = expected;
      } else if (delta > hole_threshold) {
        // A genuine hole between frames: keep the capture value.
      } else if (std::llabs(delta - nominal) > options_.drift_tolerance_us) {
        // Small deviation from the cadence: snap back to the nominal interval
        // so downstream time bases stay stable.
        ++stats.drift_fixes;
        ts = expected;
      }
    } else {
      ++stats.missing_timestamps;
      ts = estimate_missing(i, frames, unwrapped, nominal, previous_ts);
    }

    if (ts <= previous_ts) {
      ts = previous_ts + 1;
    }
    frame.ts_us = ts;
    previous_ts = ts;
  }

  for (size_t i = 1; i < frames.size(); ++i) {
    if (frames[i].ts_us <= frames[i - 1].ts_us) {
      stats.strictly_increasing = false;
      break;
    }
  }
  return stats;
}

// Estimates a missing timestamp from the nearest valid neighbours. The value
// is interpolated by sequence distance so a burst loss inside a run of missing
// clocks still spreads the frames evenly across the hole.
int64_t TimestampRepair::estimate_missing(
    size_t index, const std::vector<RepairedFrame>& frames,
    const std::vector<int64_t>& unwrapped, int64_t nominal,
    int64_t previous_ts) const {
  const uint64_t seq = frames[index].seq;
  // Nearest valid predecessor.
  size_t before = index;
  while (before > 0 && unwrapped[before - 1] < 0) {
    --before;
  }
  const bool have_before = before > 0 || unwrapped[before] >= 0;
  // Nearest valid successor.
  size_t after = index + 1;
  while (after < frames.size() && unwrapped[after] < 0) {
    ++after;
  }
  const bool have_after = after < frames.size();

  if (have_before && have_after) {
    const size_t prev_index = before - 1;
    const int64_t prev_ts = unwrapped[prev_index];
    const int64_t next_ts = unwrapped[after];
    const int64_t span_seq =
        static_cast<int64_t>(frames[after].seq - frames[prev_index].seq);
    const int64_t offset_seq =
        static_cast<int64_t>(seq - frames[prev_index].seq);
    if (span_seq > 0 && next_ts > prev_ts) {
      return prev_ts + (next_ts - prev_ts) * offset_seq / span_seq;
    }
  }
  if (have_after) {
    const int64_t distance =
        static_cast<int64_t>(frames[after].seq - seq);
    return unwrapped[after] - distance * nominal;
  }
  return (index == 0 ? 0 : previous_ts) + nominal;
}

}  // namespace mfs
