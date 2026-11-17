// SPDX-License-Identifier: MIT
#include <catch2/catch_test_macros.hpp>

#include "timecode/parser.hpp"
#include "timecode/rate_table.hpp"

using namespace tc;

TEST_CASE("rational arithmetic stays exact", "[rate]") {
    Rational r(30000, 1001);
    REQUIRE(r * Rational(1001, 30000) == Rational(1, 1));
    REQUIRE(Rational(5, 2).round() == 3);
    REQUIRE(Rational(-5, 2).round() == -3);
    REQUIRE(Rational(1, 3).round() == 0);
}

TEST_CASE("rate table uses exact integer parameters", "[rate]") {
    const auto& df = rate_info(Rate::F2997_DF);
    REQUIRE(df.drop_frame);
    REQUIRE(df.frames_per_second == Rational(30000, 1001));
    REQUIRE(df.frames_per_day == 2'589'408);
    const auto& ndf = rate_info(Rate::F2997_NDF);
    REQUIRE(!ndf.drop_frame);
    REQUIRE(ndf.frames_per_day == 2'592'000);
    REQUIRE(parse_rate("29.97-df") == Rate::F2997_DF);
    REQUIRE(parse_rate("2997NDF") == Rate::F2997_NDF);
    REQUIRE_FALSE(parse_rate("12fps").has_value());
}

TEST_CASE("strict parser accepts canonical forms", "[parser]") {
    auto a = parse_timecode("01:00:00;02", Rate::F2997_DF);
    REQUIRE(a.ok);
    REQUIRE(a.tc.hour == 1);
    REQUIRE(a.tc.frame == 2);
    REQUIRE(a.separator == Separator::Semicolon);

    auto b = parse_timecode("2@23:59:59:29", Rate::F2997_NDF);
    REQUIRE(b.ok);
    REQUIRE(b.tc.day == 2);
    REQUIRE(b.tc.hour == 23);

    auto c = parse_timecode_auto("00:00:00;00");
    REQUIRE(c.ok);
    REQUIRE(c.tc.minute == 0);
}

TEST_CASE("parser rejects structural and range errors", "[parser]") {
    REQUIRE(parse_timecode("", Rate::F2997_DF).diag == Diag::EmptyLine);
    REQUIRE(parse_timecode("01000002", Rate::F2997_DF).diag == Diag::BadSeparator);
    REQUIRE(parse_timecode("1:00:00;02", Rate::F2997_DF).diag == Diag::BadNumber);
    REQUIRE(parse_timecode("01-00-00;02", Rate::F2997_DF).diag == Diag::FieldCount);
    REQUIRE(parse_timecode("24:00:00;00", Rate::F2997_DF).diag == Diag::OutOfRange);
    REQUIRE(parse_timecode("01:00:00;30", Rate::F2997_DF).diag == Diag::OutOfRange);
    // NDF rate with DF marker and vice versa.
    REQUIRE(parse_timecode("01:00:00;02", Rate::F2997_NDF).diag == Diag::SeparatorMismatch);
    REQUIRE(parse_timecode("01:00:00:02", Rate::F2997_DF).diag == Diag::SeparatorMismatch);
    REQUIRE(parse_timecode("-1@00:00:00;00", Rate::F2997_DF).diag == Diag::NegativeDuration);
}

TEST_CASE("illegal drop-frame landings are structured errors", "[parser]") {
    // Minute 01 is a drop minute: 00 and 01 do not exist.
    REQUIRE(parse_timecode("00:01:00;00", Rate::F2997_DF).diag == Diag::IllegalDropFrame);
    REQUIRE(parse_timecode("00:01:00;01", Rate::F2997_DF).diag == Diag::IllegalDropFrame);
    // Minute 10 is exempt: 00 and 01 are legal there.
    REQUIRE(parse_timecode("00:10:00;00", Rate::F2997_DF).ok);
    REQUIRE(parse_timecode("00:00:00;01", Rate::F2997_DF).ok);
    // NDF has no illegal landings.
    REQUIRE(parse_timecode("00:01:00:00", Rate::F2997_NDF).ok);
}

TEST_CASE("malformed UTF-8 is rejected", "[parser]") {
    std::string bad = "00:01:00;\xff\xfe";
    REQUIRE(parse_timecode(bad, Rate::F2997_DF).diag == Diag::BadUtf8);
}

TEST_CASE("format round-trips through the parser", "[parser]") {
    Timecode tc{3, 12, 34, 56, 17};
    std::string text = format_timecode(tc, Rate::F2997_DF);
    REQUIRE(text == "3@12:34:56;17");
    auto back = parse_timecode(text, Rate::F2997_DF);
    REQUIRE(back.ok);
    REQUIRE(back.tc.day == 3);
    REQUIRE(back.tc.frame == 17);
}
