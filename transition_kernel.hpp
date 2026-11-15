#pragma once

#include "framebuffer.hpp"

#include <cstdint>
#include <string>
#include <vector>

namespace bake {

struct KernelParams {
    uint64_t seed = 0;
    int64_t frame = 0;
};

using KernelFunction = void (*)(const FrameBuffer& front, const FrameBuffer& back,
                                FrameBuffer& destination, uint32_t progress_q16,
                                const KernelParams& params);

struct KernelEntry {
    std::string name;
    KernelFunction function;
};

void registerBlendKernels(std::vector<KernelEntry>& entries);
void registerGeometryKernels(std::vector<KernelEntry>& entries);

const std::vector<KernelEntry>& kernelRegistry();
const KernelEntry* findKernel(const std::string& name);
void applyKernel(const std::string& name, const FrameBuffer& front,
                 const FrameBuffer& back, FrameBuffer& destination,
                 uint32_t progress_q16, const KernelParams& params);

void copyFrame(const FrameBuffer& source, FrameBuffer& destination);
uint8_t blendChannelQ16(uint8_t front, uint8_t back, uint32_t progress_q16);
uint32_t blendQ16(uint32_t front, uint32_t back, uint32_t progress_q16);
uint32_t smoothstepQ16(uint32_t progress_q16);

}  // namespace bake
