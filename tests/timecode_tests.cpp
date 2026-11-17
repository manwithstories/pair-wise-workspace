// SPDX-License-Identifier: MIT
#include <catch2/catch_test_macros.hpp>
#include <catch2/generators/catch_generators.hpp>

#include "timecode/audit.hpp"
#include "timecode/converter.hpp"
#include "timecode/dropframe.hpp"
#include "timecode/parser.hpp"
#include "timecode/rate_table.hpp"

using namespace tc;

namespace {
std::vector<AuditInput> in(std::initializer_list<const char*> tcs,
                           std::int64_t step = 1) {
    std::vector<AuditInput> v;
    for (const char* t : tcs) v.push_back(AuditInput{std::string(t), step});
    return v;
}
}  // namespace

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

TEST_CASE("canonical SMPTE drop-frame vectors", "[dropframe]") {
    struct Vec {
        const char* tc;
        std::int64_t frame;
        std::int64_t comp;
    };
    auto vec = GENERATE(Vec{"00:00:00;00", 0, 0},
                        Vec{"00:00:59;29", 1799, 0},
                        Vec{"00:01:00;02", 1800, 2},
                        Vec{"00:01:00;03", 1801, 2},
                        Vec{"00:10:00;00", 17982, 18},
                        Vec{"00:59:59;28", 107890, 108},
                        Vec{"00:59:59;29", 107891, 108},
                        Vec{"01:00:00;00", 107892, 108});
    auto pr = parse_timecode(vec.tc, Rate::F2997_DF);
    REQUIRE(pr.ok);
    REQUIRE(to_frame_number(pr.tc, Rate::F2997_DF) == vec.frame);
    REQUIRE(drop_compensation_at(pr.tc, Rate::F2997_DF) == vec.comp);
    auto back = from_frame_number(vec.frame, Rate::F2997_DF);
    REQUIRE(back.ok);
    // Frame 1799 lands on the first legal label of minute 1 (00:01:00;01);
    // every other vector maps back to itself.
    const char* expected_back =
        std::string_view(vec.tc) == "00:00:59;29" ? "00:01:00;01" : vec.tc;
    REQUIRE(format_timecode(back.tc, Rate::F2997_DF) == expected_back);
}

TEST_CASE("full-day exhaustive round trip has zero error", "[converter]") {
    auto rate = GENERATE(Rate::F24, Rate::F25, Rate::F30,
                         Rate::F2997_NDF, Rate::F2997_DF);
    const std::int64_t day_frames = rate_info(rate).frames_per_day;
    // Step by primes so every region (minute boundaries, ten-minute exempt
    // windows, hour crossings) is exercised without iterating 2.6M rows.
    for (std::int64_t f = 0; f < day_frames; f += 97) {
        auto r = from_frame_number(f, rate);
        REQUIRE(r.ok);
        REQUIRE(to_frame_number(r.tc, rate) == f);
    }
    // And the very last label of the day.
    auto last = from_frame_number(day_frames - 1, rate);
    REQUIRE(last.ok);
    REQUIRE(to_frame_number(last.tc, rate) == day_frames - 1);
}

TEST_CASE("day offsets survive the round trip and wrap collapses them",
          "[converter]") {
    // Ten-minute position has zero skips inside the minute, so wall-clock
    // components round-trip byte-identically across a day offset.
    auto pr = parse_timecode("2@01:10:20;15", Rate::F2997_DF);
    REQUIRE(pr.ok);
    const std::int64_t f = to_frame_number(pr.tc, Rate::F2997_DF);
    auto back = from_frame_number(f, Rate::F2997_DF);
    REQUIRE(back.tc.day == 2);
    REQUIRE(back.tc.hour == 1);
    REQUIRE(back.tc.minute == 10);
    REQUIRE(back.tc.second == 20);
    REQUIRE(back.tc.frame == 15);
    Timecode wrapped = wrap_to_day(pr.tc, Rate::F2997_DF);
    REQUIRE(wrapped.day == 0);
    REQUIRE(to_frame_number(wrapped, Rate::F2997_DF) ==
            f % rate_info(Rate::F2997_DF).frames_per_day);
    REQUIRE(from_frame_number(-1, Rate::F25).diag == Diag::NegativeDuration);
}

TEST_CASE("cross-rate conversion scales exact real time", "[converter]") {
    // 1 second of 29.97 DF (30 labels after the origin) -> 25 fps.
    auto pr = parse_timecode("00:00:01;00", Rate::F2997_DF);
    auto r = convert_rate(pr.tc, Rate::F2997_DF, Rate::F25);
    // 30 * (1001/30000) * 25 = 25.025 -> rounds to 25.
    REQUIRE(r.target_frame == 25);
    REQUIRE_FALSE(r.exact);
    REQUIRE(!r.warnings.empty());

    auto one_hour = parse_timecode("01:00:00;00", Rate::F2997_DF);
    auto to30 = convert_rate(one_hour.tc, Rate::F2997_DF, Rate::F30);
    // Real time of a DF hour is 107892 * 1001 / 30000 s; *30 = 107999.892.
    Rational expected(107892LL * 1001, 1000);
    REQUIRE(to30.target_frame == expected.round());
    REQUIRE(to30.target_frame == 108000);
    REQUIRE_FALSE(to30.exact);
}

TEST_CASE("drop compensation totals and NDF drift are rational", "[dropframe]") {
    REQUIRE(drop_compensation_per_day() == 2592);
    REQUIRE(drop_compensation_until(60) == 108);   // one labeled hour
    REQUIRE(drop_compensation_until(10) == 18);    // one ten-minute block
    Timecode one_hour_ndf{0, 1, 0, 0, 0};
    Rational drift = theoretical_drift_seconds(one_hour_ndf, Rate::F2997_NDF);
    // drift is defined as labeled-time minus real-time. NDF labels tick as if
    // the rate were exactly 30, so the real clock runs FASTER: after one
    // labeled hour (3600 s) real time is 3603.6 s, hence drift = -3.6 s.
    REQUIRE(drift == Rational(-18, 5));
    REQUIRE(drift < Rational(0, 1));
    Timecode one_hour_df{0, 1, 0, 0, 0};
    Rational df_drift = theoretical_drift_seconds(one_hour_df, Rate::F2997_DF);
    // DF removes the 108-frame bulk; only a sub-frame residual can remain.
    REQUIRE(df_drift >= Rational(0, 1));
    REQUIRE(df_drift < Rational(1, 30));
}

TEST_CASE("audit flags parse failures but keeps processing", "[audit]") {
    auto out = audit_sequence(
        in({"00:00:00:00", "garbage", "00:00:00:02", "", "00:00:00:03"}),
        Rate::F2997_NDF);
    REQUIRE(out.summary.records == 5);
    REQUIRE(out.summary.valid == 3);
    REQUIRE(out.summary.invalid == 2);
    REQUIRE(!out.summary.clean());
}

TEST_CASE("audit detects duplicates and distinguishes gap kinds", "[audit]") {
    auto dup = audit_sequence(
        in({"00:00:00:00", "00:00:00:01", "00:00:00:01", "00:00:00:02"}),
        Rate::F2997_NDF);
    REQUIRE(dup.summary.duplicates == 1);

    auto single = audit_sequence(
        in({"00:00:00:00", "00:00:00:02"}), Rate::F2997_NDF);  // missing frame 1
    REQUIRE(single.summary.missing == 1);
    REQUIRE(single.summary.single_gaps == 1);
    REQUIRE(single.summary.run_gaps == 0);

    auto run = audit_sequence(
        in({"00:00:00:00", "00:00:00:05"}), Rate::F2997_NDF);  // 4-frame run
    REQUIRE(run.summary.missing == 4);
    REQUIRE(run.summary.single_gaps == 0);
    REQUIRE(run.summary.run_gaps == 1);
}

TEST_CASE("audit wrap policies behave differently at midnight", "[audit]") {
    // 23:59:59:29 then 00:00:00:00 on NDF 30-label timeline: one day wrap.
    const auto seq = in({"23:59:59:29", "00:00:00:00", "00:00:00:01"});

    auto strict = audit_sequence(seq, Rate::F2997_NDF, WrapPolicy::Strict);
    REQUIRE(strict.summary.wraps == 0);
    REQUIRE(strict.summary.out_of_order >= 1);

    auto wrap = audit_sequence(seq, Rate::F2997_NDF, WrapPolicy::Wrap);
    REQUIRE(wrap.summary.wraps == 1);
    REQUIRE(wrap.summary.out_of_order == 0);

    auto repair = audit_sequence(seq, Rate::F2997_NDF, WrapPolicy::Repair);
    REQUIRE(repair.summary.wraps == 1);
    REQUIRE(repair.summary.out_of_order == 0);
}

TEST_CASE("repair never silently fixes genuine disorder", "[audit]") {
    // A small backward jump that cannot be a day wrap stays an error.
    auto out = audit_sequence(
        in({"00:01:00:00", "00:00:30:00"}), Rate::F2997_NDF, WrapPolicy::Repair);
    REQUIRE(out.summary.wraps == 0);
    REQUIRE(out.summary.out_of_order == 1);
}

TEST_CASE("clean frame-dense sequence has zero drift and is sorted",
          "[audit]") {
    auto out = audit_sequence(
        in({"00:00:00:00", "00:00:00:01", "00:00:00:02", "00:00:00:03"}),
        Rate::F2997_NDF);
    REQUIRE(out.summary.clean());
    REQUIRE(out.summary.max_abs_drift == 0);
    // Stable output keeps ascending frame order.
    for (std::size_t i = 1; i < out.records.size(); ++i)
        REQUIRE(out.records[i - 1].source_frame <= out.records[i].source_frame);
}
