#pragma once
#include <cstddef>

#include "interpolator.hpp"
#include "report.hpp"

// Pixel-by-pixel comparison of two renders of the same image.
//
// Extracted from the compare subcommand so the diff logic — first offending
// coordinate, channel, and values — can be tested without fabricating a
// divergent render path (the two real paths agree by construction; exit code 5
// exists for the day they stop agreeing, e.g. after a vectorisation change).
namespace lutgrade {

CompareReport compare_rendered(const RenderOutput& left, const RenderOutput& right,
                               int width, double threshold);

}  // namespace lutgrade
