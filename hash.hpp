#pragma once

#include "framebuffer.hpp"

#include <array>
#include <cstdint>
#include <string>

namespace bake {

constexpr std::size_t kDiffHistogramBins = 16;

struct FrameDiffStats {
    uint64_t sum_abs = 0;
    uint64_t sample_count = 0;
    uint32_t max_abs = 0;
    uint32_t changed_pixels = 0;
    std::array<uint32_t, kDiffHistogramBins> histogram{};

    uint32_t meanAbsDiff() const {
        return sample_count == 0
                   ? 0
                   : static_cast<uint32_t>((sum_abs + sample_count / 2) / sample_count);
    }
};

uint64_t fnv1a64(const FrameBuffer& frame);
std::string hashHex(uint64_t value);
FrameDiffStats diffFrames(const FrameBuffer& a, const FrameBuffer& b);

}  // namespace bake
