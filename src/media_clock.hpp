// media_clock.hpp -- shared rational time-base rules for mediaqc.
//
// Every timestamp in the pipeline is an integer count of ticks in an explicit
// rational base.  Floating point seconds are never accumulated: conversions
// between bases use av_rescale_q_rnd (integer math, round half away from
// zero), and all cross-track comparisons happen in a canonical 1/90000 base.
#pragma once

#include <cstdint>
#include <string>

extern "C" {
#include <libavutil/avutil.h>
#include <libavutil/mathematics.h>
#include <libavutil/rational.h>
}

namespace mediaqc {

// Canonical base used whenever tracks must be compared or combined.
inline constexpr AVRational kCanonicalBase = {1, 90000};
inline constexpr int64_t kCanonicalTicksPerSecond = 90000;

// Largest value of a 32-bit timestamp field: the rollover period used for
// streams whose writer stored times in 32-bit units of its time base.
inline constexpr int64_t kRolloverPeriod = int64_t{1} << 32;

struct Rational {
  int32_t num = 0;
  int32_t den = 1;

  constexpr Rational() = default;
  constexpr Rational(int32_t n, int32_t d) : num(n), den(d) {}

  static constexpr Rational from_av(AVRational r) { return Rational{r.num, r.den}; }
  constexpr AVRational av() const { return AVRational{num, den}; }
  constexpr bool valid() const { return num > 0 && den > 0; }
  constexpr bool operator==(const Rational& o) const { return num == o.num && den == o.den; }
  std::string str() const { return std::to_string(num) + "/" + std::to_string(den); }
};

// Exact integer conversion of a tick count between two rational bases.
// Rounding is nearest, half away from zero; AV_NOPTS_VALUE passes through.
inline int64_t rescale_ticks(int64_t ticks, Rational from, Rational to) {
  if (ticks == AV_NOPTS_VALUE) return ticks;
  if (!from.valid() || !to.valid() || from == to) return ticks;
  return av_rescale_q_rnd(ticks, from.av(), to.av(),
                          static_cast<AVRounding>(AV_ROUND_NEAR_INF | AV_ROUND_PASS_MINMAX));
}

// Convert a whole number of seconds into ticks of `base` (limits and CLI only).
inline int64_t seconds_to_ticks(int64_t seconds, Rational base) {
  if (!base.valid()) return 0;
  return av_rescale_q(seconds, AVRational{1, 1}, base.av());
}

// Canonical-tick helpers used by reports.  Formatting only; never fed back
// into timestamp arithmetic.
inline int64_t canonical_seconds(int64_t ticks) { return ticks / kCanonicalTicksPerSecond; }
inline int64_t canonical_millis(int64_t ticks) {
  return av_rescale_q(ticks, kCanonicalBase, AVRational{1, 1000});
}

// Rollover policy for non-monotonic DTS repair.  `period_ticks` is expressed
// in the *stream* time base: a writer that stored 32-bit fields wraps every
// 2^32 ticks.  A backwards jump is treated as a rollover only when the
// previous timestamp sits in the upper half of the period and the jump is
// larger than the threshold; this keeps ordinary small regressions distinct
// from a wrap.
struct WrapPolicy {
  bool enabled = true;
  int64_t period_ticks = 0;     // 0 -> kRolloverPeriod
  int64_t threshold_ticks = 0;  // 0 -> period_ticks / 2

  int64_t period() const { return period_ticks > 0 ? period_ticks : kRolloverPeriod; }
  int64_t threshold() const {
    if (threshold_ticks > 0) return threshold_ticks;
    return period() / 2;
  }
};

// Human-readable tick value, e.g. "1234 ticks (1/90000)".
inline std::string format_ticks(int64_t ticks, Rational base) {
  if (ticks == AV_NOPTS_VALUE) return "NOPTS";
  return std::to_string(ticks) + "@" + base.str();
}

}  // namespace mediaqc
