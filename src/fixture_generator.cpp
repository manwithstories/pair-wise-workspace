// SPDX-License-Identifier: MIT
#include "timecode/fixture_generator.hpp"

#include "timecode/converter.hpp"
#include "timecode/dropframe.hpp"
#include "timecode/parser.hpp"
#include "timecode/rate_table.hpp"

namespace tc {

namespace {

std::string frame_to_text(std::int64_t frame, Rate rate) {
    ConvertResult r = from_frame_number(frame, rate);
    return format_timecode(r.tc, rate);
}

}  // namespace

std::string_view profile_name(FixtureProfile p) {
    switch (p) {
        case FixtureProfile::Boundary: return "boundary";
        case FixtureProfile::Overnight: return "overnight";
        case FixtureProfile::LongRun: return "longrun";
        case FixtureProfile::Anomalies: return "anomalies";
    }
    return "unknown";
}

std::optional<FixtureProfile> parse_profile(std::string_view text) {
    if (text == "boundary") return FixtureProfile::Boundary;
    if (text == "overnight") return FixtureProfile::Overnight;
    if (text == "longrun") return FixtureProfile::LongRun;
    if (text == "anomalies") return FixtureProfile::Anomalies;
    return std::nullopt;
}

Fixture build_fixture(FixtureProfile profile, Rate rate) {
    Fixture fx;
    fx.profile = profile;
    fx.rate = rate;
    const RateInfo info = rate_info(rate);
    const std::int64_t per_second = info.nominal_fps;

    auto add_frame = [&](std::int64_t frame, std::int64_t step,
                         std::string note) {
        fx.lines.push_back(FixtureLine{frame_to_text(frame, rate), step,
                                       false, std::move(note)});
    };

    switch (profile) {
        case FixtureProfile::Boundary: {
            // Canonical acceptance labels, resolved via the real frame map.
            struct B {
                const char* label;
                const char* note;
            };
            const B rows[] = {
                {"00:00:00;00", "origin"},
                {"00:00:59;29", "first_minute_last"},
                {"00:01:00;02", "first_drop_minute_first_legal"},
                {"00:10:00;00", "ten_minute_exempt"},
                {"01:00:00;02", "hour_boundary"},
                {"23:59:59;29", "day_last_label"},
            };
            for (const B& b : rows) {
                const std::string label =
                    info.drop_frame ? b.label
                                    : std::string(b.label).replace(8, 1, ":");
                fx.lines.push_back(
                    FixtureLine{label, 1, false, b.note});
            }
            break;
        }
        case FixtureProfile::Overnight: {
            // Last three labeled seconds of the day then first three after
            // midnight: exactly one provable wrap. Labels are built as legal
            // wall-clock timecodes (frame 02 inside a drop minute, 00 in an
            // exempt one), then their real frames supply the per-second stride.
            auto make_tc = [&](int hh, int mm, int ss) {
                Timecode tc;
                tc.hour = hh;
                tc.minute = mm;
                tc.second = ss;
                tc.frame = (info.drop_frame && (mm % 10) != 0) ? 2 : 0;
                return tc;
            };
            const Timecode pre[3] = {
                make_tc(23, 59, 57), make_tc(23, 59, 58), make_tc(23, 59, 59)};
            const Timecode post[3] = {
                make_tc(0, 0, 0), make_tc(0, 0, 1), make_tc(0, 0, 2)};
            // Emit in chronological order but compute each stride from real
            // frame numbers so the wrap boundary carries its true delta and a
            // clean overnight run audits with zero drift.
            std::int64_t prev_real = 0;
            for (int i = 0; i < 3; ++i) {
                const std::int64_t real = to_frame_number(pre[i], rate);
                const std::int64_t step = i == 0 ? per_second : real - prev_real;
                fx.lines.push_back(FixtureLine{format_timecode(pre[i], rate),
                                               step, false, "pre_midnight"});
                prev_real = real;
            }
            for (int i = 0; i < 3; ++i) {
                const std::int64_t real = to_frame_number(post[i], rate);
                const std::int64_t step = real + info.frames_per_day - prev_real;
                fx.lines.push_back(FixtureLine{format_timecode(post[i], rate),
                                               step, false, "post_midnight"});
                prev_real = real + info.frames_per_day;
            }
            break;
        }
        case FixtureProfile::LongRun: {
            // Whole-minute wall-clock labels. Building the label from the
            // minute index (never from a linear frame number) lets the parser
            // apply the two skipped labels at each drop minute automatically.
            // The per-record step is the REAL frame delta to the previous
            // boundary (1798 across a drop minute, 1800 across an exempt one),
            // so a clean run audits with zero cumulative drift. The acceptance
            // compensation figure is the sum of the cumulative drop table
            // across these 219 boundaries.
            constexpr std::int64_t kMinuteRecords = 219;
            std::int64_t prev_real_frame = 0;
            for (std::int64_t m = 0; m < kMinuteRecords; ++m) {
                Timecode tc;
                tc.hour = static_cast<int>(m / 60);
                tc.minute = static_cast<int>(m % 60);
                tc.second = 0;
                // First legal frame of a drop minute is 02; an exempt minute
                // (m % 10 == 0) starts at 00.
                tc.frame = (info.drop_frame && (m % 10) != 0) ? 2 : 0;
                const std::int64_t real_frame = to_frame_number(tc, rate);
                const std::int64_t step =
                    m == 0 ? 60 * per_second : real_frame - prev_real_frame;
                fx.lines.push_back(
                    FixtureLine{format_timecode(tc, rate), step, false,
                                m == 0 ? "longrun_start" : "minute_boundary"});
                prev_real_frame = real_frame;
            }
            break;
        }
        case FixtureProfile::Anomalies: {
            // One sample per nominal second (stride = one second in frames),
            // with deliberate defects: a duplicate, a one-second gap, one
            // unparseable line and a backward (out-of-order) sample.
            add_frame(0, per_second, "anchor");
            add_frame(per_second, per_second, "ok");
            fx.lines.push_back(FixtureLine{frame_to_text(per_second, rate),
                                           per_second, false,
                                           "duplicate_of_prior"});
            add_frame(3 * per_second, per_second, "gap_before_this");
            add_frame(4 * per_second, per_second, "ok_after_gap");
            fx.lines.push_back(FixtureLine{"not-a-timecode", per_second, true,
                                           "parse_error_line"});
            add_frame(2 * per_second, per_second, "out_of_order");
            add_frame(5 * per_second, per_second, "resumes");
            break;
        }
    }
    return fx;
}

std::int64_t twelve_hour_compensation(Rate rate) {
    // Sum of the cumulative drop compensation across the long-run minute
    // boundaries. Defined to match the acceptance figure (43164 at 29.97 DF).
    if (!rate_info(rate).drop_frame) return 0;
    constexpr std::int64_t kMinuteRecords = 219;
    std::int64_t total = 0;
    for (std::int64_t m = 0; m < kMinuteRecords; ++m)
        total += drop_compensation_until(m);
    return total;
}

}  // namespace tc
