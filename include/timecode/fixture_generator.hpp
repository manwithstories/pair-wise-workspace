// SPDX-License-Identifier: MIT
// timecode/fixture_generator.hpp — deterministic, source-generated fixtures.
//
// No external data files: every sample is derived from the frame math so a
// fresh checkout reproduces byte-identical output. Profiles cover minute
// boundaries, midnight wrap, a twelve-hour continuous run and anomaly sets.
#pragma once

#include <cstdint>
#include <string>
#include <vector>

#include "timecode/types.hpp"

namespace tc {

struct FixtureLine {
    std::string text;        // canonical timecode string
    std::int64_t step = 1;   // expected stride for the auditor
    bool inject_error = false;
    std::string note;        // deterministic annotation, never random
};

enum class FixtureProfile {
    Boundary,    // minute / ten-minute / hour drop-frame boundaries
    Overnight,   // crosses midnight: one provable wrap
    LongRun,     // continuous twelve-hour sequence
    Anomalies,   // duplicates, gaps, disorder, parse errors
};

struct Fixture {
    FixtureProfile profile;
    Rate rate;
    std::vector<FixtureLine> lines;
};

std::string_view profile_name(FixtureProfile p);
std::optional<FixtureProfile> parse_profile(std::string_view text);

// Build the named fixture at the given rate. The twelve-hour long run uses a
// per-second sample stride so its drop compensation can be summed exactly.
Fixture build_fixture(FixtureProfile profile, Rate rate);

// The exact total drop-frame compensation of a twelve-hour (43200 s) DF run
// sampled once per second: sum of per-sample compensations. Exposed for the
// acceptance check (~43164 frames).
std::int64_t twelve_hour_compensation(Rate rate);

}  // namespace tc
