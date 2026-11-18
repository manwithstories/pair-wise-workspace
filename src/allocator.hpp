#pragma once

#include <cstdint>
#include <vector>

// Allocates an integer bit budget across frames, proportional to
// weight_i = 1 + 3 * intensity_i, using the largest-remainder method:
// each frame gets floor(ideal_i), then the leftover units go one each to
// the largest fractional remainders; ties are broken by lower index.
// The returned allocations always sum to exactly `budget`.
//
// The intensities must come from the shared diff kernel (diff.hpp);
// this function never recomputes frame differences itself.
std::vector<uint64_t> allocate_bits(const std::vector<double>& intensities,
                                    uint64_t budget);
