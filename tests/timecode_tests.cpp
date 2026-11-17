// SPDX-License-Identifier: MIT
#include <catch2/catch_test_macros.hpp>

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
