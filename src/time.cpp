#include "mediacore/time.hpp"

#include <algorithm>
#include <array>
#include <charconv>
#include <cstdlib>
#include <limits>

namespace mediacore {

namespace {
bool mulOverflow(int64_t a, int64_t b, int64_t& out) {
#if defined(__GNUC__) || defined(__clang__)
    return __builtin_mul_overflow(a, b, &out);
#else
    if (a != 0 && std::abs(b) > std::numeric_limits<int64_t>::max() / std::abs(a))
        return true;
    out = a * b;
    return false;
#endif
}
}  // namespace

std::optional<int64_t> lcmChecked(int64_t a, int64_t b) {
    a = std::abs(a);
    b = std::abs(b);
    if (a == 0 || b == 0) return 0;
    const int64_t g = std::gcd(a, b);
    int64_t product;
    if (mulOverflow(a / g, b, product)) return std::nullopt;
    return product;
}

std::optional<Rational> commonTimebase(const Rational& a,
                                       const Rational& b) {
    auto lcm = lcmChecked(a.den, b.den);
    if (!lcm) return std::nullopt;
    return Rational::reduced(1, *lcm);
}

std::optional<Tick> scaleExact(Tick t, const Rational& from,
                               const Rational& to) {
    if (from.den <= 0 || to.den <= 0 || from.num <= 0 || to.num <= 0)
        return std::nullopt;
    // t * (from.num/from.den) / (to.num/to.den)
    // = t * from.num * to.den / (from.den * to.num)
    int64_t numPart, denPart;
    if (mulOverflow(t, from.num, numPart)) return std::nullopt;
    if (mulOverflow(numPart, to.den, numPart)) return std::nullopt;
    if (mulOverflow(from.den, to.num, denPart)) return std::nullopt;
    if (denPart == 0) return std::nullopt;
    if (numPart % denPart != 0) return std::nullopt;
    return numPart / denPart;
}

int compareTick(Tick a, const Rational& aBase,
                Tick b, const Rational& bBase) {
    int64_t lhs, rhs;
    if (mulOverflow(a, aBase.num, lhs) ||
        mulOverflow(b, bBase.num, rhs))
        throw std::overflow_error("timestamp comparison overflow");
    // compare lhs/aBase.den vs rhs/bBase.den
    if (mulOverflow(lhs, bBase.den, lhs) ||
        mulOverflow(rhs, aBase.den, rhs))
        throw std::overflow_error("timestamp comparison overflow");
    if (lhs < rhs) return -1;
    if (lhs > rhs) return 1;
    return 0;
}

std::optional<Rational> parseDecimal(std::string_view text) {
    while (!text.empty() && text.front() == ' ') text.remove_prefix(1);
    while (!text.empty() && text.back() == ' ') text.remove_suffix(1);
    if (text.empty()) return std::nullopt;
    int64_t whole = 0;
    int64_t frac = 0;
    int64_t scale = 1;
    auto* p = text.data();
    auto* end = text.data() + text.size();
    if (*p == '-') return std::nullopt;  // durations are non-negative here
    auto [ptr, ec] = std::from_chars(p, end, whole);
    if (ec != std::errc{}) return std::nullopt;
    if (ptr != end) {
        if (*ptr != '.') return std::nullopt;
        ++ptr;
        if (ptr == end) return std::nullopt;
        for (auto* q = ptr; q != end; ++q) {
            if (*q < '0' || *q > '9') return std::nullopt;
            frac = frac * 10 + (*q - '0');
            scale *= 10;
        }
    }
    return Rational::reduced(whole * scale + frac, scale);
}

bool overlaps(const TimeRange& a, const TimeRange& b) {
    return compareTick(a.start, a.base, b.start, b.base) < 0
               ? compareTick(b.start, b.base, a.end, a.base) < 0
               : compareTick(a.start, a.base, b.end, b.base) < 0;
}

std::optional<TimeRange> scaleRangeExact(const TimeRange& r,
                                         const Rational& to) {
    auto s = scaleExact(r.start, r.base, to);
    auto e = scaleExact(r.end, r.base, to);
    if (!s || !e) return std::nullopt;
    return TimeRange{*s, *e, to};
}

std::string toString(const Rational& r) {
    return std::to_string(r.num) + "/" + std::to_string(r.den);
}

std::string toString(const TimeRange& r) {
    return "[" + std::to_string(r.start) + "," + std::to_string(r.end) +
           ")@" + toString(r.base);
}

}  // namespace mediacore
