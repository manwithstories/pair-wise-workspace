#include "rational.hpp"

#include <cerrno>
#include <cstdlib>
#include <limits>
#include <numeric>

namespace media_restore {
namespace {

__extension__ using int128 = __int128;
constexpr int128 kMaxI64 = std::numeric_limits<int64_t>::max();
constexpr int128 kMinI64 = std::numeric_limits<int64_t>::min();

bool fits(int128 value) { return value <= kMaxI64 && value >= kMinI64; }

Rational normalized(int128 num, int128 den) {
  if (den == 0) return Rational();
  if (den < 0) {
    num = -num;
    den = -den;
  }
  const int128 divisor = std::gcd(num < 0 ? -num : num, den);
  if (divisor > 1) {
    num /= divisor;
    den /= divisor;
  }
  Rational result;
  result.num = static_cast<int64_t>(num);
  result.den = static_cast<int64_t>(den);
  return result;
}

}  // namespace

Rational::Rational(int64_t numerator, int64_t denominator) {
  if (denominator == 0) {
    num = 0;
    den = 0;
    return;
  }
  *this = normalized(numerator, denominator);
}

namespace {

// strtoll silently saturates on overflow, which would turn a hostile or
// corrupt manifest value into INT64_MAX.  Parse with explicit range checks so
// an out-of-range time is rejected instead of quietly clamped.
bool parse_int64(const std::string& text, int64_t* out) {
  if (text.empty()) return false;
  errno = 0;
  char* end = nullptr;
  const long long value = std::strtoll(text.c_str(), &end, 10);
  if (errno == ERANGE || end != text.c_str() + text.size()) return false;
  *out = static_cast<int64_t>(value);
  return true;
}

}  // namespace

bool Rational::parse(std::string_view text, Rational* out) {
  const size_t slash = text.find('/');
  if (slash == std::string_view::npos) {
    int64_t value = 0;
    if (!parse_int64(std::string(text), &value)) return false;
    *out = Rational(value, 1);
    return true;
  }
  int64_t numerator = 0;
  int64_t denominator = 0;
  if (!parse_int64(std::string(text.substr(0, slash)), &numerator) ||
      !parse_int64(std::string(text.substr(slash + 1)), &denominator) ||
      denominator == 0) {
    return false;
  }
  *out = Rational(numerator, denominator);
  return out->valid();
}

std::string Rational::to_string() const {
  return std::to_string(num) + "/" + std::to_string(den);
}

bool Rational::floor_ticks(const Rational& time, const Rational& tick,
                           int64_t* out) {
  if (!time.valid() || !tick.valid() || tick.num <= 0) return false;
  const int128 numerator = static_cast<int128>(time.num) * tick.den;
  const int128 denominator = static_cast<int128>(time.den) * tick.num;
  int128 quotient = numerator / denominator;
  if (numerator % denominator != 0 && numerator < 0) --quotient;
  if (!fits(quotient)) return false;
  *out = static_cast<int64_t>(quotient);
  return true;
}

bool Rational::exact_ticks(const Rational& time, const Rational& tick,
                           int64_t* out) {
  if (!time.valid() || !tick.valid() || tick.num <= 0) return false;
  const int128 numerator = static_cast<int128>(time.num) * tick.den;
  const int128 denominator = static_cast<int128>(time.den) * tick.num;
  if (numerator % denominator != 0) return false;
  const int128 quotient = numerator / denominator;
  if (!fits(quotient)) return false;
  *out = static_cast<int64_t>(quotient);
  return true;
}

bool Rational::sample_count(const Rational& time, int64_t sample_rate,
                            int64_t* out) {
  if (sample_rate <= 0) return false;
  return exact_ticks(time, Rational(1, sample_rate), out);
}

bool Rational::nearest_ticks(const Rational& time, const Rational& tick,
                             int64_t* ticks, Rational* remainder) {
  if (!time.valid() || !tick.valid() || tick.num <= 0) return false;
  const int128 numerator = static_cast<int128>(time.num) * tick.den;
  const int128 denominator = static_cast<int128>(time.den) * tick.num;
  if (denominator == 0) return false;
  int128 quotient = numerator / denominator;
  const int128 r = numerator % denominator;
  if (r != 0) {
    // Round to nearest, ties away from zero, using integer magnitudes.
    const int128 abs_r = r < 0 ? -r : r;
    if (abs_r * 2 >= denominator) quotient += numerator < 0 ? -1 : 1;
  }
  if (!fits(quotient)) return false;
  *ticks = static_cast<int64_t>(quotient);
  // The exact leftover in the original time units: (time - ticks * tick).
  const int128 leftover_num = numerator - quotient * denominator;
  const int128 leftover_den = denominator;
  if (!fits(leftover_num) || !fits(leftover_den)) return false;
  // numerator/denominator is in tick units, so convert back by multiplying
  // both sides out: leftover = (num * tick.num) / (den * tick.den).
  const int128 num = leftover_num * tick.num;
  const int128 den = leftover_den * tick.den;
  if (!fits(num) || !fits(den)) return false;
  *remainder = Rational(static_cast<int64_t>(num), static_cast<int64_t>(den));
  return true;
}

bool Rational::add(const Rational& a, const Rational& b, Rational* out) {
  if (!a.valid() || !b.valid()) return false;
  const int128 num = static_cast<int128>(a.num) * b.den +
                       static_cast<int128>(b.num) * a.den;
  const int128 den = static_cast<int128>(a.den) * b.den;
  if (!fits(num) || !fits(den) || den == 0) return false;
  *out = normalized(num, den);
  return true;
}

bool Rational::subtract(const Rational& a, const Rational& b, Rational* out) {
  if (!b.valid() || b.num == std::numeric_limits<int64_t>::min()) return false;
  return add(a, Rational(-b.num, b.den), out);
}

bool Rational::compare(const Rational& a, const Rational& b, int* out) {
  if (!a.valid() || !b.valid()) return false;
  const int128 left = static_cast<int128>(a.num) * b.den;
  const int128 right = static_cast<int128>(b.num) * a.den;
  *out = left < right ? -1 : (left > right ? 1 : 0);
  return true;
}

double Rational::to_double() const {
  return static_cast<double>(num) / static_cast<double>(den);
}

}  // namespace media_restore
