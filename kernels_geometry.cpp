#include "transition_kernel.hpp"

#include <algorithm>
#include <cstdint>

namespace bake {

namespace {

uint32_t mix32(uint32_t value) {
    value ^= value >> 16;
    value *= 0x7feb352dU;
    value ^= value >> 15;
    value *= 0x846ca68bU;
    value ^= value >> 16;
    return value;
}

int64_t clampQ16(int64_t value, int maximum) {
    const int64_t upper = static_cast<int64_t>(maximum) << 16;
    return std::max<int64_t>(0, std::min(value, upper));
}

void sampleBilinear(const FrameBuffer& frame, int64_t x_q16, int64_t y_q16,
                    uint8_t& red, uint8_t& green, uint8_t& blue) {
    const int64_t clamped_x = clampQ16(x_q16, frame.width - 1);
    const int64_t clamped_y = clampQ16(y_q16, frame.height - 1);
    const int x0 = static_cast<int>(clamped_x >> 16);
    const int y0 = static_cast<int>(clamped_y >> 16);
    const int x1 = std::min(x0 + 1, frame.width - 1);
    const int y1 = std::min(y0 + 1, frame.height - 1);
    const uint32_t fx = static_cast<uint32_t>(clamped_x & 0xffff);
    const uint32_t fy = static_cast<uint32_t>(clamped_y & 0xffff);
    uint8_t p00[3] = {}, p10[3] = {}, p01[3] = {}, p11[3] = {};
    frame.getPixel(x0, y0, p00[0], p00[1], p00[2]);
    frame.getPixel(x1, y0, p10[0], p10[1], p10[2]);
    frame.getPixel(x0, y1, p01[0], p01[1], p01[2]);
    frame.getPixel(x1, y1, p11[0], p11[1], p11[2]);
    uint8_t result[3] = {};
    for (int channel = 0; channel < 3; ++channel) {
        const uint32_t top = blendQ16(p00[channel], p10[channel], fx);
        const uint32_t bottom = blendQ16(p01[channel], p11[channel], fx);
        result[channel] = static_cast<uint8_t>(blendQ16(top, bottom, fy));
    }
    red = result[0];
    green = result[1];
    blue = result[2];
}

void geometryResample(const FrameBuffer& front, const FrameBuffer& back,
                      FrameBuffer& destination, uint32_t progress,
                      const KernelParams& params) {
    for (int y = 0; y < front.height; ++y) {
        for (int x = 0; x < front.width; ++x) {
            const uint32_t threshold = mix32(
                static_cast<uint32_t>(x) * 0x9e3779b9U ^
                static_cast<uint32_t>(y) * 0x85ebca6bU ^
                static_cast<uint32_t>(params.seed)) >> 16;
            const FrameBuffer& source = progress >= threshold ? back : front;
            uint8_t r = 0, g = 0, b = 0;
            sampleBilinear(source, (static_cast<int64_t>(x) << 16) + 16384,
                           (static_cast<int64_t>(y) << 16) + 16384, r, g, b);
            destination.setPixel(x, y, r, g, b);
        }
    }
}

void blockPermute(const FrameBuffer& front, const FrameBuffer& back,
                  FrameBuffer& destination, uint32_t progress,
                  const KernelParams& params) {
    constexpr int block_size = 8;
    for (int y = 0; y < front.height; ++y) {
        for (int x = 0; x < front.width; ++x) {
            const uint32_t bx = static_cast<uint32_t>(x / block_size);
            const uint32_t by = static_cast<uint32_t>(y / block_size);
            const uint32_t key = mix32(bx * 0x9e3779b9U ^ by * 0x85ebca6bU ^
                                       static_cast<uint32_t>(params.seed));
            const uint32_t threshold = key >> 16;
            const FrameBuffer& source = progress >= threshold ? back : front;
            uint8_t r = 0, g = 0, b = 0;
            source.getPixel(x, y, r, g, b);
            destination.setPixel(x, y, r, g, b);
        }
    }
}

}  // namespace

void registerGeometryKernels(std::vector<KernelEntry>& entries) {
    entries.push_back({"geometry_resample", &geometryResample});
    entries.push_back({"block_permute", &blockPermute});
}

}  // namespace bake
