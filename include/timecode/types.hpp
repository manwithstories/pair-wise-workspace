// SPDX-License-Identifier: MIT
// timecode/types.hpp — core value types, diagnostics and rational arithmetic.
// All frame math in this project is integer / rational; no float is ever used
// to decide a frame number.
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <vector>

namespace tc {

// ---------------------------------------------------------------- rational --

// Signed exact rational, always kept reduced, denominator strictly positive.
struct Rational {
    std::int64_t num = 0;
    std::int64_t den = 1;

    Rational() = default;
    Rational(std::int64_t n, std::int64_t d);
    explicit Rational(std::int64_t n) : num(n), den(1) {}

    static Rational raw(std::int64_t n, std::int64_t d) {  // already reduced
        Rational r;
        r.num = n;
        r.den = d;
        return r;
    }

    Rational operator+(const Rational& o) const;
    Rational operator-(const Rational& o) const;
    Rational operator*(const Rational& o) const;
    Rational operator/(const Rational& o) const;
    bool operator==(const Rational& o) const;
    bool operator<(const Rational& o) const;
    bool operator<=(const Rational& o) const;
    bool operator>(const Rational& o) const;
    bool operator>=(const Rational& o) const;

    // Round-half-away-from-zero to an integer (used for cross-rate conversion).
    std::int64_t round() const;
    std::int64_t floor() const;
};

// --------------------------------------------------------------- enums -----

enum class Rate {
    F24,        // 24/1   NDF, integer rate
    F25,        // 25/1   NDF, integer rate
    F30,        // 30/1   NDF, integer rate
    F2997_NDF,  // 30000/1001 nominal 30, non-drop-frame
    F2997_DF,   // 30000/1001 drop-frame
};

enum class Separator { Colon, Semicolon };  // ':' vs ';' before the frame field

enum class WrapPolicy {
    Strict,  // any out-of-order / wrap is a hard data error
    Wrap,    // accept backward jumps as day wraps, count them
    Repair,  // prove wraps (backward jump near a full day) and re-index;
             // genuine disorder is still reported, never silently reordered
};

// Structured diagnostic codes. Every parse/audit failure carries one.
enum class Diag : int {
    Ok = 0,
    EmptyLine,
    FieldCount,
    BadUtf8,
    BadNumber,
    BadSeparator,
    SeparatorMismatch,   // ';' on an NDF rate or ':' on a DF rate
    IllegalDropFrame,    // frame 00/01 at a dropped boundary
    OutOfRange,          // component outside its legal range
    NegativeDuration,
    FrameOverflow,       // beyond the representable day frame count
    BadJson,
    UnknownFormat,
    OutOfOrder,
    DuplicateFrame,
    MissingFrame,
    UnprovenWrap,        // repair could not prove a backward jump was a day wrap
};

const char* diag_name(Diag d) noexcept;

// ------------------------------------------------------------ value types --

struct RateInfo {
    Rate rate;
    const char* canonical;   // stable wire name, e.g. "2997df"
    bool drop_frame;
    std::int64_t nominal_fps;          // 24, 25, 30
    Rational frames_per_second;       // exact, e.g. 30000/1001
    std::int64_t frames_per_day;      // labeled-frame count of one TC day
};

struct Timecode {
    std::int64_t day = 0;       // explicit day offset, >= 0
    int hour = 0;               // 0..23
    int minute = 0;             // 0..59
    int second = 0;             // 0..59
    int frame = 0;              // 0..nominal_fps-1 (and never an illegal DF slot)
};

struct ParseResult {
    bool ok = false;
    Timecode tc;
    Diag diag = Diag::Ok;
    std::string message;
    std::string raw;           // original input string, preserved verbatim
    Separator separator = Separator::Colon;
};

struct AuditEvent {
    std::size_t line = 0;      // 1-based input line
    Diag diag = Diag::Ok;
    std::string message;
    std::string raw;
};

struct Record {
    std::size_t line = 0;
    std::string raw;
    bool valid = false;
    Diag diag = Diag::Ok;
    std::string message;
    Timecode tc;
    std::int64_t source_frame = 0;       // labeled absolute frame (day-aware)
    std::int64_t ideal_frame = 0;        // monotonic position in the valid chain
    std::int64_t drop_compensation = 0;  // cumulative dropped numbers before it
    std::int64_t cumulative_drift = 0;   // ideal vs actual, in frames
    Rational real_time_seconds;          // exact elapsed real time (rational)
};

}  // namespace tc
