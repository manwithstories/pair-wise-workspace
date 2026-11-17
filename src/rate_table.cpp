// SPDX-License-Identifier: MIT
#include "timecode/rate_table.hpp"

#include <array>
#include <cctype>
#include <cstdlib>
#include <stdexcept>

namespace tc {

// ------------------------------------------------------------- rational ----

std::int64_t gcd_i64(std::int64_t a, std::int64_t b) noexcept {
    if (a < 0) a = -a;
    if (b < 0) b = -b;
    while (b != 0) {
        std::int64_t t = a % b;
        a = b;
        b = t;
    }
    return a;
}

std::int64_t div_floor(std::int64_t a, std::int64_t b) noexcept {
    // b is always positive in this code base.
    std::int64_t q = a / b;
    std::int64_t r = a % b;
    if (r != 0 && ((a < 0) != (b < 0))) --q;
    return q;
}

Rational::Rational(std::int64_t n, std::int64_t d) {
    if (d == 0) throw std::invalid_argument("rational denominator is zero");
    if (d < 0) {
        n = -n;
        d = -d;
    }
    std::int64_t g = gcd_i64(n, d);
    if (g == 0) g = 1;
    num = n / g;
    den = d / g;
}

Rational Rational::operator+(const Rational& o) const {
    return Rational(num * o.den + o.num * den, den * o.den);
}
Rational Rational::operator-(const Rational& o) const {
    return Rational(num * o.den - o.num * den, den * o.den);
}
Rational Rational::operator*(const Rational& o) const {
    return Rational(num * o.num, den * o.den);
}
Rational Rational::operator/(const Rational& o) const {
    return Rational(num * o.den, den * o.num);
}
bool Rational::operator==(const Rational& o) const {
    return num * o.den == o.num * den;
}
bool Rational::operator<(const Rational& o) const {
    return num * o.den < o.num * den;
}
bool Rational::operator<=(const Rational& o) const { return !(o < *this); }
bool Rational::operator>(const Rational& o) const { return o < *this; }

std::int64_t Rational::round() const {
    // Half away from zero.
    if (num >= 0) return (num + den / 2) / den;
    return -((-num + den / 2) / den);
}

std::int64_t Rational::floor() const { return div_floor(num, den); }

// ------------------------------------------------------------- diagnostics -

const char* diag_name(Diag d) noexcept {
    switch (d) {
        case Diag::Ok: return "ok";
        case Diag::EmptyLine: return "empty_line";
        case Diag::FieldCount: return "field_count";
        case Diag::BadUtf8: return "bad_utf8";
        case Diag::BadNumber: return "bad_number";
        case Diag::BadSeparator: return "bad_separator";
        case Diag::SeparatorMismatch: return "separator_mismatch";
        case Diag::IllegalDropFrame: return "illegal_drop_frame";
        case Diag::OutOfRange: return "out_of_range";
        case Diag::NegativeDuration: return "negative_duration";
        case Diag::FrameOverflow: return "frame_overflow";
        case Diag::BadJson: return "bad_json";
        case Diag::UnknownFormat: return "unknown_format";
        case Diag::OutOfOrder: return "out_of_order";
        case Diag::DuplicateFrame: return "duplicate_frame";
        case Diag::MissingFrame: return "missing_frame";
        case Diag::UnprovenWrap: return "unproven_wrap";
    }
    return "unknown";
}

// ------------------------------------------------------------- rate table --

namespace {

constexpr std::int64_t k2997Nominal = 30;

// Labeled frame count of one 29.97 DF day:
//   nominal 30 fps day minus 2 frames for every non-ten minute (9 per 10).
constexpr std::int64_t df_day_frames() {
    return k2997Nominal * 60 * 60 * 24 - 2 * 9 * 6 * 24;  // 2 589 408
}

const std::array<RateInfo, 5>& table() {
    static const std::array<RateInfo, 5> rows = {{
        {Rate::F24, "24", false, 24, Rational::raw(24, 1), 24 * 86400},
        {Rate::F25, "25", false, 25, Rational::raw(25, 1), 25 * 86400},
        {Rate::F30, "30", false, 30, Rational::raw(30, 1), 30 * 86400},
        {Rate::F2997_NDF, "2997ndf", false, k2997Nominal,
         Rational::raw(30000, 1001), k2997Nominal * 86400},
        {Rate::F2997_DF, "2997df", true, k2997Nominal,
         Rational::raw(30000, 1001), df_day_frames()},
    }};
    return rows;
}

std::string normalize_name(std::string_view text) {
    std::string out;
    for (char c : text) {
        if (c == '.' || c == '-' || c == '_' || c == ' ') continue;
        out.push_back(static_cast<char>(std::tolower(static_cast<unsigned char>(c))));
    }
    return out;
}

}  // namespace

std::optional<Rate> parse_rate(std::string_view text) {
    const std::string key = normalize_name(text);
    if (key == "24" || key == "24fps") return Rate::F24;
    if (key == "25" || key == "25fps") return Rate::F25;
    if (key == "30" || key == "30fps") return Rate::F30;
    if (key == "2997ndf" || key == "2997n" || key == "2997") return Rate::F2997_NDF;
    if (key == "2997df" || key == "2997d") return Rate::F2997_DF;
    return std::nullopt;
}

const RateInfo& rate_info(Rate rate) {
    for (const auto& row : table()) {
        if (row.rate == rate) return row;
    }
    throw std::invalid_argument("unknown rate enum");
}

const std::vector<RateInfo>& all_rates() {
    static const std::vector<RateInfo> rows(table().begin(), table().end());
    return rows;
}

}  // namespace tc
