// SPDX-License-Identifier: MIT
#include "timecode/converter.hpp"

#include <fmt/format.h>

#include "timecode/dropframe.hpp"
#include "timecode/rate_table.hpp"

namespace tc {

namespace {
constexpr std::int64_t kFramesPerMinuteLocal = 30 * 60;  // labeled 30-based
}  // namespace

std::int64_t to_frame_number(const Timecode& tc, Rate rate) {
    const RateInfo info = rate_info(rate);
    const std::int64_t base =
        ((tc.hour * 60LL + tc.minute) * 60 + tc.second) * info.nominal_fps + tc.frame;
    // frames_per_day already nets the full-day skips, so the day term needs no
    // extra compensation; only the within-day drop skips are subtracted here.
    const std::int64_t day_frames = tc.day * info.frames_per_day;
    if (info.drop_frame) {
        const std::int64_t within_day_minutes = tc.hour * 60LL + tc.minute;
        return day_frames + base - drop_compensation_until(within_day_minutes);
    }
    return day_frames + base;
}

ConvertResult from_frame_number(std::int64_t frame, Rate rate) {
    const RateInfo info = rate_info(rate);
    if (frame < 0) {
        return {false, Diag::NegativeDuration,
                fmt::format("frame {} is before the timecode origin", frame), 0, {}};
    }
    const std::int64_t day = frame / info.frames_per_day;
    std::int64_t f = frame % info.frames_per_day;

    Timecode tc;
    tc.day = day;
    if (info.drop_frame) {
        // Invert F = 1800*m + s*30 + ff - 2*(m - m/10) by reconstructing the
        // minute. The first frame of minute m is start(m); pick the unique m
        // with start(m) <= f < start(m+1), correcting the closed-form seed.
        auto minute_start = [](std::int64_t m) {
            return m * kFramesPerMinuteLocal - drop_compensation_until(m);
        };
        // Exact seed: ignoring skips, m = floor(f/1800); bias by one to land
        // just above the boundary and let the while-loops settle deterministically.
        std::int64_t minute = (f + 2) / kFramesPerMinuteLocal;
        while (minute > 0 && f < minute_start(minute)) --minute;
        while (f >= minute_start(minute + 1)) ++minute;
        f -= minute_start(minute);
        tc.hour = static_cast<int>(minute / 60);
        tc.minute = static_cast<int>(minute % 60);
    } else {
        tc.hour = static_cast<int>(f / (3600LL * info.nominal_fps));
        f %= 3600LL * info.nominal_fps;
        tc.minute = static_cast<int>(f / (60LL * info.nominal_fps));
        f %= 60LL * info.nominal_fps;
    }
    tc.second = static_cast<int>(f / info.nominal_fps);
    tc.frame = static_cast<int>(f % info.nominal_fps);
    return {true, Diag::Ok, {}, frame, tc};
}

Timecode wrap_to_day(const Timecode& tc, Rate rate) {
    const std::int64_t frame = to_frame_number(tc, rate);
    ConvertResult r = from_frame_number(frame % rate_info(rate).frames_per_day, rate);
    return r.tc;
}

CrossRateResult convert_rate(const Timecode& tc, Rate from, Rate to) {
    const RateInfo src = rate_info(from);
    const RateInfo dst = rate_info(to);
    CrossRateResult out;

    out.source_frame = to_frame_number(tc, from);
    // Exact real elapsed time as a rational: frames / frames_per_second.
    const Rational real_time(out.source_frame * src.frames_per_second.den,
                             src.frames_per_second.num);
    // Target frame index at the destination's exact rate.
    const Rational exact_target = real_time * dst.frames_per_second;
    out.target_frame = exact_target.round();
    out.exact = (exact_target == Rational(out.target_frame, 1));

    ConvertResult back = from_frame_number(out.target_frame, to);
    out.target_tc = back.tc;
    if (!back.ok) {
        out.warnings.push_back(back.message);
        return out;
    }
    if (!out.exact) {
        out.warnings.push_back(fmt::format(
            "rate ratio is not integral; target frame rounded ({} -> {})",
            exact_target.num, out.target_frame));
    }
    if (tc.day != 0) {
        out.warnings.push_back(
            "source carried a day offset; offset preserved on the target timeline");
    }
    return out;
}

}  // namespace tc
