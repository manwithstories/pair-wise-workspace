#pragma once

#include <cstdint>

#include "media/types.hpp"

namespace mediaqc {

// Converts one track's device counter into unified nanoseconds and repairs
// the defects suppliers ship: missing capture times, counter wraparound,
// non-monotonic values and drift against the declared frame rate.
//
// The output is a perfectly uniform grid: frame N is placed at
// `origin_ns + slot * interval_ns`, where `slot` advances by at least one
// per frame.  That guarantees the re-muxed container a strictly increasing,
// channel-consistent timeline.  The supplier value is retained as `raw_ns`
// so the repair report can show exactly what changed and why.
struct TimelineConfig {
  Rational rate;        // frames per second, must be valid
  ClockSpec clock;      // counter width and tick rate
  Ns origin_ns = 0;     // timeline value of the first frame (or clock epoch)
  // Deviation from the ideal slot before a frame is reported as drift.
  // 0 selects interval/4.
  Ns drift_tolerance_ns = 0;
};

// Persistable state so a resumed run continues exactly where it stopped.
struct TimelineState {
  bool started = false;
  bool clock_anchored = false;
  std::int64_t device_zero_ns = 0;   // device time mapped to origin_ns
  std::int64_t last_device_ns = 0;   // device time of the previous frame
  std::int64_t last_unwrapped_ticks = 0;
  std::uint64_t last_raw_ticks = 0;
  std::int64_t last_slot = 0;
  Ns last_ns = 0;
  std::uint64_t wrap_offset = 0;
  std::uint64_t frames = 0;
  std::uint64_t repaired = 0;
};

class TrackTimeline {
 public:
  explicit TrackTimeline(TimelineConfig config);

  struct Mapping {
    Ns ns = 0;          // repaired value written to the output
    Ns raw_ns = 0;      // supplier value on the unified clock, unrepaired
    bool has_raw = false;
    std::int64_t slot = 0;
    RepairMask reasons = 0;
  };

  // Maps the next frame in processing order.  `has_ticks == false` means the
  // supplier omitted the capture time and it is derived from the frame rate.
  Mapping map(bool has_ticks, std::int64_t ticks);

  [[nodiscard]] Ns interval_ns() const { return interval_ns_; }
  [[nodiscard]] Ns drift_tolerance_ns() const { return tolerance_ns_; }
  [[nodiscard]] std::uint64_t frames() const { return state_.frames; }
  [[nodiscard]] std::uint64_t repaired() const { return state_.repaired; }
  [[nodiscard]] const TimelineState& state() const { return state_; }
  void restore(const TimelineState& state);

  [[nodiscard]] static Ns compute_interval_ns(const Rational& rate);
  [[nodiscard]] static bool counter_wraps(const ClockSpec& clock);

 private:
  [[nodiscard]] std::int64_t unwrap(std::int64_t raw, RepairMask& reasons);
  [[nodiscard]] Ns device_ns(std::int64_t unwrapped_ticks) const;
  [[nodiscard]] Ns slot_to_ns(std::int64_t slot) const;

  TimelineConfig config_;
  Ns interval_ns_ = 0;
  Ns tolerance_ns_ = 0;
  TimelineState state_;
};

// Rounds `ticks * 1e9 / hz` to the nearest nanosecond.  Returns false when
// the result does not fit in int64 (only possible with a misconfigured clock).
[[nodiscard]] bool ticks_to_ns_checked(std::int64_t ticks, std::uint32_t hz,
                                       Ns& out);

}  // namespace mediaqc
