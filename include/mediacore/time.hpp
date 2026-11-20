#pragma once

#include <cstdint>
#include <numeric>
#include <optional>
#include <stdexcept>
#include <string>
#include <string_view>

namespace mediacore {

// Unified rational time base. All timestamps are integer ticks on an
// explicit Rational scale (ticks * num/den == seconds). Conversion between
// scales must stay exact: scaleExact() refuses non-divisible mappings
// instead of silently rounding.
struct Rational {
    int64_t num = 0;
    int64_t den = 1;

    static Rational reduced(int64_t n, int64_t d) {
        if (d == 0) throw std::invalid_argument("Rational with zero denominator");
        if (d < 0) { n = -n; d = -d; }
        const int64_t g = std::gcd(std::abs(n), d);
        return {n / g, d / g};
    }
};

using Tick = int64_t;

// lcm of two positive denominators, refusing overflow.
std::optional<int64_t> lcmChecked(int64_t a, int64_t b);

// A time base of 1 tick per lcm denominator, used for cross-track compares.
std::optional<Rational> commonTimebase(const Rational& a, const Rational& b);

// Exact rescaling: returns nullopt when t cannot be mapped onto `to`
// without a remainder (caller reports NonDivisibleScale).
std::optional<Tick> scaleExact(Tick t, const Rational& from,
                               const Rational& to);

// Compare two timestamps living on different scales, exactly (cross
// multiplication, no floating point). Returns -1, 0, 1.
int compareTick(Tick a, const Rational& aBase,
                Tick b, const Rational& bBase);

// Parse a decimal string ("1.000", "48000", "0.968") into an exact
// rational. Returns nullopt on malformed input.
std::optional<Rational> parseDecimal(std::string_view text);

struct TimeRange {
    Tick start = 0;
    Tick end = 0;
    Rational base{0, 1};

    Tick length() const { return end - start; }
    bool empty() const { return end <= start; }
};

bool overlaps(const TimeRange& a, const TimeRange& b);

// Map a range onto another scale; both endpoints must convert exactly.
std::optional<TimeRange> scaleRangeExact(const TimeRange& r,
                                         const Rational& to);

std::string toString(const Rational& r);
std::string toString(const TimeRange& r);

}  // namespace mediacore
