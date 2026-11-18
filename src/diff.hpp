#pragma once

#include <cstdint>

#include "frame.hpp"

// Frame-difference kernel. This is the single definition of motion
// intensity; statistics, segmentation and the budget allocator all
// consume values produced here.
namespace diff {

// ITU-R style integer luma: (77R + 150G + 29B) >> 8.
inline uint8_t luma(uint8_t r, uint8_t g, uint8_t b) {
    return static_cast<uint8_t>((77u * r + 150u * g + 29u * b) >> 8);
}

// Sum of per-pixel absolute luma differences. Both frames must have
// identical dimensions and 3 bytes per pixel. Row padding is excluded.
// Throws std::invalid_argument on a dimension mismatch.
uint64_t abs_luma_sum(const FrameBuffer& a, const FrameBuffer& b);

// Motion intensity in [0,1]: abs_luma_sum / (255 * W * H).
double intensity(const FrameBuffer& a, const FrameBuffer& b);

} // namespace diff
