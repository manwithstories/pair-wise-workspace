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

uint32_t pixelThreshold(int x, int y, const KernelParams& params) {
    const uint32_t position = static_cast<uint32_t>(x) * 0x9e3779b9U ^
                              static_cast<uint32_t>(y) * 0x85ebca6bU;
    return mix32(position ^ static_cast<uint32_t>(params.seed)) >> 16;
}

void linearBlend(const FrameBuffer& front, const FrameBuffer& back,
                 FrameBuffer& destination, uint32_t progress, const KernelParams&) {
    for (int y = 0; y < front.height; ++y) {
        for (int x = 0; x < front.width; ++x) {
            uint8_t ar = 0, ag = 0, ab = 0;
            uint8_t br = 0, bg = 0, bb = 0;
            front.getPixel(x, y, ar, ag, ab);
            back.getPixel(x, y, br, bg, bb);
            destination.setPixel(x, y, blendChannelQ16(ar, br, progress),
                                 blendChannelQ16(ag, bg, progress),
                                 blendChannelQ16(ab, bb, progress));
        }
    }
}

void thresholdMask(const FrameBuffer& front, const FrameBuffer& back,
                   FrameBuffer& destination, uint32_t progress,
                   const KernelParams& params) {
    for (int y = 0; y < front.height; ++y) {
        for (int x = 0; x < front.width; ++x) {
            const FrameBuffer& source = progress >= pixelThreshold(x, y, params)
                                            ? back
                                            : front;
            uint8_t r = 0, g = 0, b = 0;
            source.getPixel(x, y, r, g, b);
            destination.setPixel(x, y, r, g, b);
        }
    }
}

void channelKey(const FrameBuffer& front, const FrameBuffer& back,
                FrameBuffer& destination, uint32_t progress,
                const KernelParams& params) {
    for (int y = 0; y < front.height; ++y) {
        for (int x = 0; x < front.width; ++x) {
            uint8_t r = 0, g = 0, b = 0;
            front.getPixel(x, y, r, g, b);
            const uint32_t key_threshold = static_cast<uint32_t>(
                (static_cast<uint32_t>(g) * 257U +
                 (pixelThreshold(x, y, params) & 0xffU)) & 0xffffU);
            const FrameBuffer& source = progress >= key_threshold ? back : front;
            uint8_t sr = 0, sg = 0, sb = 0;
            source.getPixel(x, y, sr, sg, sb);
            destination.setPixel(x, y, sr, sg, sb);
        }
    }
}

}  // namespace

void registerBlendKernels(std::vector<KernelEntry>& entries) {
    entries.push_back({"linear_blend", &linearBlend});
    entries.push_back({"threshold_mask", &thresholdMask});
    entries.push_back({"channel_key", &channelKey});
}

}  // namespace bake
