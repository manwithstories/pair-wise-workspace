#pragma once

// Procedural frame content for the sample generator.
//
// A moving gradient with a bright border on keyframes: deterministic, cheap to
// encode, and visually distinct between frames. The pattern is a pure function
// of (size, index, keyframe) so the same seed reproduces the same pixels.

#include <cstdint>
#include <vector>

namespace mfs {

std::vector<uint8_t> render_pattern(uint32_t width, uint32_t height,
                                    uint32_t index, bool keyframe);

}  // namespace mfs
