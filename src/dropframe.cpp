// SPDX-License-Identifier: MIT
#include "timecode/dropframe.hpp"

#include "timecode/rate_table.hpp"

namespace tc {

bool is_dropped_label(int minute, int frame) {
    return (minute % 10) != 0 && (frame == 0 || frame == 1);
}

std::int64_t drop_compensation_until(std::int64_t total_minutes_so_far) {
    // Cumulative skips of the `total_minutes_so_far` whole minutes *before*
    // the current position — i.e. minute indices 0..m-1. Each index not
    // divisible by ten skips two labels: 2 * (m - floor(m/10)).
    if (total_minutes_so_far <= 0) return 0;
    const std::int64_t m = total_minutes_so_far;
    return 2 * (m - m / 10);
}

std::int64_t drop_compensation_at(const Timecode& tc, Rate rate) {
    if (!rate_info(rate).drop_frame) return 0;
    // Skips contributed by every whole minute before the current one. Within
    // the current minute a parsed label can never land on 00/01, so the
    // current minute adds nothing.
    const std::int64_t elapsed_minutes =
        tc.day * 24 * 60 + tc.hour * 60 + tc.minute;
    return drop_compensation_until(elapsed_minutes);
}

std::int64_t drop_compensation_per_day() {
    return drop_compensation_until(24 * 60);  // 2592
}

Rational theoretical_drift_seconds(const Timecode& tc, Rate rate) {
    const RateInfo info = rate_info(rate);
    // Labeled elapsed time treats every label as 1/nominal second.
    const std::int64_t labeled_seconds =
        tc.day * 86400LL + ((tc.hour * 60 + tc.minute) * 60 + tc.second);
    const Rational labeled_time(labeled_seconds * info.nominal_fps + tc.frame,
                                 info.nominal_fps);
    // Real elapsed time is actual frames (labels minus skips) at the true rate.
    const std::int64_t skipped = drop_compensation_at(tc, rate);
    const std::int64_t actual_frames =
        labeled_seconds * info.nominal_fps + tc.frame - skipped;
    const Rational real_time(actual_frames * info.frames_per_second.den,
                             info.frames_per_second.num);
    // Positive drift: labels are ahead of the real clock.
    return labeled_time - real_time;
}

}  // namespace tc
