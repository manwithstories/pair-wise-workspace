#include "transition_kernel.hpp"

#include <algorithm>

namespace bake {

namespace {

constexpr uint32_t kOne = 65536U;

std::vector<KernelEntry> buildRegistry() {
    std::vector<KernelEntry> entries;
    registerBlendKernels(entries);
    registerGeometryKernels(entries);
    for (std::size_t i = 0; i < entries.size(); ++i) {
        if (entries[i].name.empty() || entries[i].function == nullptr) {
            throw BakeError(ExitCode::invariant, "kernel registry has an empty entry");
        }
        for (std::size_t j = i + 1; j < entries.size(); ++j) {
            if (entries[i].name == entries[j].name) {
                throw BakeError(ExitCode::invariant,
                                "duplicate kernel registration: " + entries[i].name);
            }
        }
    }
    return entries;
}

}  // namespace

const std::vector<KernelEntry>& kernelRegistry() {
    static const std::vector<KernelEntry> entries = buildRegistry();
    return entries;
}

const KernelEntry* findKernel(const std::string& name) {
    const auto& entries = kernelRegistry();
    const auto it = std::find_if(entries.begin(), entries.end(),
                                 [&name](const KernelEntry& entry) {
                                     return entry.name == name;
                                 });
    return it == entries.end() ? nullptr : &*it;
}

void copyFrame(const FrameBuffer& source, FrameBuffer& destination) {
    if (!sameDimensions(source, destination)) {
        throw BakeError(ExitCode::invariant, "kernel frame dimension mismatch");
    }
    destination.copyFrom(source);
}

uint32_t blendQ16(uint32_t front, uint32_t back, uint32_t progress_q16) {
    const uint64_t inverse = kOne - progress_q16;
    return static_cast<uint32_t>(
        (static_cast<uint64_t>(front) * inverse +
         static_cast<uint64_t>(back) * progress_q16 + kOne / 2) /
        kOne);
}

uint8_t blendChannelQ16(uint8_t front, uint8_t back, uint32_t progress_q16) {
    return static_cast<uint8_t>(blendQ16(front, back, progress_q16));
}

uint32_t smoothstepQ16(uint32_t progress_q16) {
    const uint64_t t = progress_q16;
    const uint64_t t2 = t * t;
    const uint64_t t3 = t2 * t;
    return static_cast<uint32_t>((3ULL * t2 * kOne - 2ULL * t3 +
                                  static_cast<uint64_t>(kOne) * kOne / 2) /
                                 (static_cast<uint64_t>(kOne) * kOne));
}

void applyKernel(const std::string& name, const FrameBuffer& front,
                 const FrameBuffer& back, FrameBuffer& destination,
                 uint32_t progress_q16, const KernelParams& params) {
    if (!sameDimensions(front, back) || !sameDimensions(front, destination)) {
        throw BakeError(ExitCode::invariant,
                        "kernel input/output dimensions do not match" +
                            locationSuffix(params.frame));
    }
    if (progress_q16 > kOne) {
        throw BakeError(ExitCode::invalid_input,
                        "kernel progress exceeds Q16.16 range" +
                            locationSuffix(params.frame),
                        params.frame);
    }
    destination.resetInitialized();
    if (progress_q16 == 0) {
        copyFrame(front, destination);
    } else if (progress_q16 == kOne) {
        copyFrame(back, destination);
    } else {
        const KernelEntry* entry = findKernel(name);
        if (entry == nullptr) {
            throw BakeError(ExitCode::invalid_input,
                            "unknown kernel: " + name + locationSuffix(params.frame),
                            params.frame);
        }
        entry->function(front, back, destination, progress_q16, params);
    }
    if (!destination.fullyInitialized()) {
        throw BakeError(ExitCode::invariant,
                        "kernel left uninitialized pixels: " + name +
                            locationSuffix(params.frame),
                        params.frame);
    }
}

}  // namespace bake
