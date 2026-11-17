// SPDX-License-Identifier: MIT
// timecode/converter.hpp — frame-number bidirectional conversion.
//
// Every conversion is integer/rational. The same-format round trip
// tc -> frame -> tc is exact (zero error) for every legal labeled position,
// including explicit day offsets.
#pragma once

#include <optional>
#include <string>
#include <vector>

#include "timecode/types.hpp"

namespace tc {

struct ConvertResult {
    bool ok = false;
    Diag diag = Diag::Ok;
    std::string message;
    std::int64_t frame = 0;       // day-aware absolute labeled frame
    Timecode tc;
};

// Timecode -> day-aware absolute frame. The timecode is assumed to have passed
// strict parsing (legal labels only).
std::int64_t to_frame_number(const Timecode& tc, Rate rate);

// Absolute frame -> timecode. Frames beyond one labeled day carry a day
// offset instead of wrapping; negative frames are an error.
ConvertResult from_frame_number(std::int64_t frame, Rate rate);

// Modulo-24 wrap: collapse day offsets back into a single labeled day.
Timecode wrap_to_day(const Timecode& tc, Rate rate);

struct CrossRateResult {
    bool exact = false;           // true when no rounding was needed
    std::int64_t source_frame = 0;
    std::int64_t target_frame = 0;
    Timecode target_tc;
    std::vector<std::string> warnings;
};

// Convert a parsed timecode between rates by scaling exact real time.
CrossRateResult convert_rate(const Timecode& tc, Rate from, Rate to);

}  // namespace tc
