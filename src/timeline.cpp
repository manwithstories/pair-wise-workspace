#include "media/timeline.hpp"

#include <cstdlib>
#include <limits>

namespace mediaqc {
namespace {

// Round half away from zero.  Deterministic so resumed runs reproduce the
// same slot assignments bit for bit.
std::int64_t round_div(std::int64_t num, std::int64_t den) {
  if (den == 0) {
    return 0;
  }
  bool negative = (num < 0) != (den < 0);
  std::uint64_t a = static_cast<std::uint64_t>(num < 0 ? -num : num);
  std::uint64_t b = static_cast<std::uint64_t>(den < 0 ? -den : den);
  std::uint64_t q = a / b;
  std::uint64_t r = a % b;
  if (r * 2 >= b) {
    ++q;
  }
  return negative ? -static_cast<std::int64_t>(q) : static_cast<std::int64_t>(q);
}

}  // namespace

Ns TrackTimeline::compute_interval_ns(const Rational& rate) {
  if (!rate.valid()) {
    return 0;
  }
  // One frame lasts den/num seconds, i.e. 1e9 * den / num nanoseconds.
  return round_div(static_cast<std::int64_t>(kNsPerSecond) * rate.den, rate.num);
}

bool TrackTimeline::counter_wraps(const ClockSpec& clock) {
  return clock.bits > 0 && clock.bits < 64;
}

bool ticks_to_ns_checked(std::int64_t ticks, std::uint32_t hz, Ns& out) {
  if (hz == 0) {
    return false;
  }
  if (hz == static_cast<std::uint32_t>(kNsPerSecond)) {
    out = ticks;
    return true;
  }
  const std::int64_t scale = kNsPerSecond;
  if (ticks <= std::numeric_limits<std::int64_t>::max() / scale &&
      ticks >= std::numeric_limits<std::int64_t>::min() / scale) {
    out = round_div(ticks * scale, static_cast<std::int64_t>(hz));
    return true;
  }
  std::int64_t q = ticks / hz;
  std::int64_t r = ticks % hz;
  if (q > std::numeric_limits<std::int64_t>::max() / scale ||
      q < std::numeric_limits<std::int64_t>::min() / scale) {
    return false;
  }
  out = q * scale + round_div(r * scale, static_cast<std::int64_t>(hz));
  return true;
}

TrackTimeline::TrackTimeline(TimelineConfig config) : config_(config) {
  interval_ns_ = compute_interval_ns(config_.rate);
  if (interval_ns_ <= 0) {
    interval_ns_ = 1;
  }
  if (config_.clock.hz == 0) {
    config_.clock.hz = static_cast<std::uint32_t>(kNsPerSecond);
  }
  tolerance_ns_ = config_.drift_tolerance_ns > 0 ? config_.drift_tolerance_ns
                                                  : interval_ns_ / 4;
  if (tolerance_ns_ < 1) {
    tolerance_ns_ = 1;
  }
}

void TrackTimeline::restore(const TimelineState& state) { state_ = state; }

Ns TrackTimeline::device_ns(std::int64_t unwrapped_ticks) const {
  Ns ns = 0;
  if (!ticks_to_ns_checked(unwrapped_ticks, config_.clock.hz, ns)) {
    return 0;
  }
  return ns;
}

Ns TrackTimeline::slot_to_ns(std::int64_t slot) const {
  return config_.origin_ns + static_cast<Ns>(slot) * interval_ns_;
}

std::int64_t TrackTimeline::unwrap(std::int64_t raw, RepairMask& reasons) {
  if (!counter_wraps(config_.clock)) {
    return raw;
  }
  const std::uint64_t modulus = 1ULL << config_.clock.bits;
  if (raw < 0) {
    raw += static_cast<std::int64_t>(modulus);
  }
  std::int64_t unwrapped = raw + static_cast<std::int64_t>(state_.wrap_offset);
  if (state_.started) {
    const std::int64_t prev_raw = static_cast<std::int64_t>(state_.last_raw_ticks);
    // A drop of more than half the counter range is a rollover, not a
    // backwards step: the counter cannot realistically move that far.
    if (raw + static_cast<std::int64_t>(modulus / 2) < prev_raw) {
      state_.wrap_offset += modulus;
      unwrapped = raw + static_cast<std::int64_t>(state_.wrap_offset);
      reasons = reasons | RepairReason::kWraparound;
    }
  }
  return unwrapped;
}

TrackTimeline::Mapping TrackTimeline::map(bool has_ticks, std::int64_t ticks) {
  Mapping result;
  ++state_.frames;

  if (!state_.started) {
    state_.started = true;
    state_.last_slot = 0;
    result.slot = 0;
    result.ns = slot_to_ns(0);
    if (has_ticks) {
      RepairMask reasons = 0;
      const std::int64_t unwrapped = unwrap(ticks, reasons);
      state_.device_zero_ns = device_ns(unwrapped);
      state_.last_device_ns = state_.device_zero_ns;
      state_.last_unwrapped_ticks = unwrapped;
      state_.last_raw_ticks = static_cast<std::uint64_t>(ticks);
      state_.clock_anchored = true;
      result.raw_ns = config_.origin_ns;
      result.has_raw = true;
      result.reasons = reasons;
      if (reasons != 0) ++state_.repaired;
    } else {
      result.reasons = static_cast<RepairMask>(RepairReason::kMissing);
      ++state_.repaired;
    }
    return result;
  }

  if (!has_ticks) {
    // Derive the capture time from the configured frame rate.  The device
    // clock is unknown here, so the next timestamped frame must be
    // re-anchored rather than compared with a stale device reading.
    const std::int64_t slot = state_.last_slot + 1;
    state_.last_slot = slot;
    state_.clock_anchored = false;
    result.slot = slot;
    result.ns = slot_to_ns(slot);
    result.reasons = static_cast<RepairMask>(RepairReason::kMissing);
    ++state_.repaired;
    return result;
  }

  RepairMask reasons = 0;
  const std::int64_t unwrapped = unwrap(ticks, reasons);
  const Ns device_time = device_ns(unwrapped);

  if (!state_.clock_anchored) {
    // The first timestamped frame after one or more missing ones.  Map its
    // device time onto the *next* slot so the frame lands exactly where the
    // derived timeline already is, and prime the delta so this frame does
    // not report a spurious drift.
    const std::int64_t next_slot = state_.last_slot + 1;
    state_.device_zero_ns =
        device_time - static_cast<Ns>(next_slot) * interval_ns_;
    state_.last_device_ns = device_time - interval_ns_;
    state_.clock_anchored = true;
  }

  const Ns delta = device_time - state_.last_device_ns;
  std::int64_t step = round_div(delta, interval_ns_);

  if (delta <= 0) {
    // The device clock moved backwards or repeated.  Keep the output
    // timeline strictly increasing and flag the defect.
    reasons = reasons | RepairReason::kNonMonotonic;
    step = 1;
  } else if (step < 1) {
    // Less than one grid interval elapsed: snap to the next slot.
    step = 1;
  }

  const std::int64_t slot = state_.last_slot + step;
  const Ns ideal = slot_to_ns(slot);
  const Ns raw_ns = config_.origin_ns + (device_time - state_.device_zero_ns);

  if (std::llabs(raw_ns - ideal) > tolerance_ns_) {
    // The supplier clock and the declared rate disagree; the grid wins.
    reasons = reasons | RepairReason::kDrift;
  }

  state_.last_device_ns = device_time;
  state_.last_unwrapped_ticks = unwrapped;
  state_.last_raw_ticks = static_cast<std::uint64_t>(ticks);
  state_.last_slot = slot;

  result.slot = slot;
  result.ns = ideal;
  result.raw_ns = raw_ns;
  result.has_raw = true;
  result.reasons = reasons;
  if (reasons != 0) ++state_.repaired;
  return result;
}

}  // namespace mediaqc
