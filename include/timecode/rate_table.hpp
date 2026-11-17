// SPDX-License-Identifier: MIT
// timecode/rate_table.hpp — exact rational parameters for every supported rate.
#pragma once

#include <optional>
#include <string_view>

#include "timecode/types.hpp"

namespace tc {

// Canonical names: "24", "25", "30", "2997ndf", "2997df".
// Common spellings ("29.97df", "29.97-ndf", "2997NDF" …) are accepted too.
std::optional<Rate> parse_rate(std::string_view text);

const RateInfo& rate_info(Rate rate);

// All five rows, fixed order: 24, 25, 30, 29.97 NDF, 29.97 DF.
const std::vector<RateInfo>& all_rates();

// Integer helpers shared across modules.
std::int64_t gcd_i64(std::int64_t a, std::int64_t b) noexcept;
std::int64_t div_floor(std::int64_t a, std::int64_t b) noexcept;

}  // namespace tc
