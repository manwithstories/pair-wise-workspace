#pragma once

// Unified rational time base for the relay inspection pipeline.
// All modules (transport reorder, stream probe, caption parser) express
// timestamps on this single millisecond-capable rational axis so damage
// ranges can be merged across stages without rounding drift.

#include <cstdint>
#include <stdexcept>
#include <string>

namespace relay {

struct Rational {
  int64_t num = 0;
  int64_t den = 1;
};

namespace detail {
constexpr int64_t gcd_i(int64_t a, int64_t b) {
  if (a < 0) a = -a;
  if (b < 0) b = -b;
  while (b != 0) {
    int64_t t = a % b;
    a = b;
    b = t;
  }
  return a == 0 ? 1 : a;
}
inline Rational from_i128(__int128 n, __int128 d) {
  if (d == 0) throw std::invalid_argument("rational denominator is zero");
  if (d < 0) {
    n = -n;
    d = -d;
  }
  int64_t nn = static_cast<int64_t>(n);
  int64_t dd = static_cast<int64_t>(d);
  if (static_cast<__int128>(nn) != n || static_cast<__int128>(dd) != d)
    throw std::overflow_error("rational overflow");
  int64_t g2 = gcd_i(nn, dd);
  return {nn / g2, dd / g2};
}
}  // namespace detail

inline Rational make_rational(int64_t num, int64_t den) {
  return detail::from_i128(num, den);
}
inline Rational rational_add(Rational a, Rational b) {
  return detail::from_i128((__int128)a.num * b.den + (__int128)b.num * a.den,
                           (__int128)a.den * b.den);
}
inline Rational rational_sub(Rational a, Rational b) {
  return detail::from_i128((__int128)a.num * b.den - (__int128)b.num * a.den,
                           (__int128)a.den * b.den);
}
inline Rational rational_mul(Rational a, Rational b) {
  return detail::from_i128((__int128)a.num * b.num, (__int128)a.den * b.den);
}
inline Rational rational_div(Rational a, Rational b) {
  if (b.num == 0) throw std::invalid_argument("rational division by zero");
  return detail::from_i128((__int128)a.num * b.den, (__int128)a.den * b.num);
}
inline int rational_cmp(Rational a, Rational b) {
  __int128 l = (__int128)a.num * b.den;
  __int128 r = (__int128)b.num * a.den;
  return l < r ? -1 : (l > r ? 1 : 0);
}
inline bool rational_eq(Rational a, Rational b) { return rational_cmp(a, b) == 0; }
inline double rational_to_double(Rational r) {
  return static_cast<double>(r.num) / static_cast<double>(r.den);
}
// Floor of a rational as int64 (rounds toward -inf).
inline int64_t rational_floor(Rational r) {
  int64_t q = r.num / r.den;
  if (r.num % r.den != 0 && r.num < 0) --q;
  return q;
}
// Round to nearest int64, halves away from zero.
inline int64_t rational_round(Rational r) {
  Rational half{1, 2};
  if (r.num >= 0) return rational_floor(rational_add(r, half));
  return -rational_floor(rational_sub(half, r));
}

// A timestamp: ticks in a rational time base (seconds = ticks * tb.num/tb.den).
struct TimePoint {
  int64_t ticks = 0;
  Rational time_base{1, 1000};
};
inline Rational to_seconds(TimePoint t) {
  return rational_mul({t.ticks, 1}, t.time_base);
}
inline Rational to_ms(TimePoint t) {
  return rational_mul(to_seconds(t), {1000, 1});
}

// ---- Timecode standards -------------------------------------------------

enum class TimecodeStandard { Film24, Pal25, NtscNonDrop30, NtscDrop30 };

inline Rational frame_rate(TimecodeStandard s) {
  switch (s) {
    case TimecodeStandard::Film24: return {24, 1};
    case TimecodeStandard::Pal25: return {25, 1};
    case TimecodeStandard::NtscNonDrop30:
    case TimecodeStandard::NtscDrop30: return {30000, 1001};
  }
  return {25, 1};
}
inline bool is_drop_frame(TimecodeStandard s) {
  return s == TimecodeStandard::NtscDrop30;
}
inline int nominal_fps(TimecodeStandard s) {
  return s == TimecodeStandard::Film24 ? 24 : (s == TimecodeStandard::Pal25 ? 25 : 30);
}

struct FrameConversion {
  int64_t frame = 0;          // floor of the exact frame position
  Rational exact_frames{};    // exact rational frame position
  Rational error_bound_ms{};  // 0 when the conversion is exact
  bool exact = false;
};

// Milliseconds -> frame number. When the ms value does not land exactly on a
// frame boundary, the result is flagged inexact and carries an error bound;
// we never pass a rounded frame off as an exact conversion.
inline FrameConversion ms_to_frame(Rational ms, TimecodeStandard s) {
  Rational fps = frame_rate(s);
  FrameConversion out;
  out.exact_frames = rational_div(rational_mul(ms, fps), {1000, 1});
  out.frame = rational_floor(out.exact_frames);
  out.exact = out.exact_frames.den == 1;
  out.error_bound_ms = out.exact ? Rational{0, 1} : rational_div({1000, 1}, fps);
  return out;
}

// Frame number -> milliseconds, always exact.
inline Rational frame_to_ms(int64_t frame, TimecodeStandard s) {
  return rational_div({frame * 1000, 1}, frame_rate(s));
}

struct Timecode {
  int hh = 0, mm = 0, ss = 0, ff = 0;
  bool drop_frame = false;
};

// Drop-frame (30/1.001) timecode <-> frame number, SMPTE 2-frame drop per
// minute except every 10th minute.
inline int64_t timecode_to_frame(Timecode tc, TimecodeStandard s) {
  int fps = nominal_fps(s);
  int64_t total_minutes = static_cast<int64_t>(tc.hh) * 60 + tc.mm;
  int64_t frames =
      ((static_cast<int64_t>(tc.hh) * 3600 + tc.mm * 60 + tc.ss) * fps) + tc.ff;
  if (tc.drop_frame && is_drop_frame(s))
    frames -= 2 * (total_minutes - total_minutes / 10);
  return frames;
}

inline Timecode frame_to_timecode(int64_t frame, TimecodeStandard s) {
  Timecode tc;
  tc.drop_frame = is_drop_frame(s);
  int fps = nominal_fps(s);
  if (tc.drop_frame) {
    // Inverse of the drop-frame mapping. A 10-minute block holds 17982 real
    // frames: minute 0 has 1800, minutes 1..9 have 1798 each.
    const int64_t frames_per_10min = 17982;
    const int64_t frames_per_min = 1798;
    int64_t d = frame / frames_per_10min;
    int64_t m = frame % frames_per_10min;
    int64_t minute_index = m < 1800 ? 0 : 1 + (m - 1800) / frames_per_min;
    int64_t adj = frame + 2 * (9 * d + minute_index);
    tc.hh = static_cast<int>(adj / (fps * 3600));
    tc.mm = static_cast<int>((adj / (fps * 60)) % 60);
    tc.ss = static_cast<int>((adj / fps) % 60);
    tc.ff = static_cast<int>(adj % fps);
    return tc;
  }
  tc.hh = static_cast<int>(frame / (fps * 3600));
  tc.mm = static_cast<int>((frame / (fps * 60)) % 60);
  tc.ss = static_cast<int>((frame / fps) % 60);
  tc.ff = static_cast<int>(frame % fps);
  return tc;
}

inline std::string to_string(Timecode tc) {
  char buf[16];
  std::snprintf(buf, sizeof(buf), "%02d:%02d:%02d%c%02d", tc.hh, tc.mm, tc.ss,
                tc.drop_frame ? ';' : ':', tc.ff);
  return buf;
}

}  // namespace relay
