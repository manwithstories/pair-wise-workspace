// SPDX-License-Identifier: MIT
// timecode/dropframe.hpp — 29.97 drop-frame arithmetic (pure integer math).
#pragma once

#include <cstdint>

#include "timecode/types.hpp"

namespace tc {

// Frames whose labels are skipped by time k (in labeled 30-based frame units):
//   two per drop minute, nine drop minutes per ten.
// This is the cumulative drop-frame compensation for one labeled position.
std::int64_t drop_compensation_until(std::int64_t total_minutes_so_far);

// Same value for a concrete wall-clock position. Returns 0 for NDF rates.
std::int64_t drop_compensation_at(const Timecode& tc, Rate rate);

// Total skips across a whole labeled day: 2 * 9 * 6 * 24 = 2592 frames.
std::int64_t drop_compensation_per_day();

// Theoretical drift between labeled time and real elapsed time.
// NDF 29.97 labels run slow; DF compensates but retains a sub-frame residual.
// Returned as exact rational seconds.
Rational theoretical_drift_seconds(const Timecode& tc, Rate rate);

// True when (minute, frame) is one of the two skipped labels.
bool is_dropped_label(int minute, int frame);

}  // namespace tc
