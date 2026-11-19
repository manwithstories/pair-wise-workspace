#pragma once

#include <cstdint>
#include <string>
#include <string_view>

namespace media_restore {

// Media time is always an exact rational tick count.  Floating point is only
// ever produced at display time; all comparisons and boundaries stay integer.
struct Rational {
  int64_t num = 0;
  int64_t den = 1;

  Rational() = default;
  Rational(int64_t numerator, int64_t denominator);

  static bool parse(std::string_view text, Rational* out);
  std::string to_string() const;

  bool valid() const { return den > 0; }
  bool is_zero() const { return num == 0; }

  // Integer tick count at `tick` resolution, rounding toward negative
  // infinity.  Returns false when the result does not fit in int64_t.
  static bool floor_ticks(const Rational& time, const Rational& tick,
                          int64_t* out);
  // Exact tick count; fails when `time` is not an integer multiple of `tick`.
  static bool exact_ticks(const Rational& time, const Rational& tick,
                          int64_t* out);
  // Exact duration in a sample rate's units.
  static bool sample_count(const Rational& time, int64_t sample_rate,
                           int64_t* out);

  // Checked arithmetic; returns false on overflow or a zero denominator.
  static bool add(const Rational& a, const Rational& b, Rational* out);
  static bool subtract(const Rational& a, const Rational& b, Rational* out);
  static bool compare(const Rational& a, const Rational& b, int* out);
  // Display-only conversion.  Never use for boundary decisions.
  double to_double() const;
};

}  // namespace media_restore
